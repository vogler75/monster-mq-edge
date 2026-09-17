# Plan: RTSP Stream Camera Bridge & Round-Robin Snapshots for MonsterMQ Edge

## Executive Summary

This plan introduces an **RTSP Camera Bridge** to `monster-mq-edge` (Go broker) and its paired **iX Web Dashboard** (`monster-mq-dashboard`). 

The bridge connects to RTSP camera streams, extracts Motion JPEG (MJPEG) frames in **100% pure Go (zero CGO)**, and publishes snapshots into a configurable number of **round-robin slot topics** (e.g. `cameras/front_gate/capture/frames/1` and `cameras/front_gate/capture/frames/1/meta`). Snapshots can be captured either **continuously (overwriting on a timer)**, or **on-demand (triggered via an MQTT topic)**.

---

## 1. Key Objectives & Architectural Constraints

1. **Pure Go (Zero CGO)**:
   - Must strictly maintain `CGO_ENABLED=0` to support fast, single-binary cross-compilation for Raspberry Pi 4/5 (ARM64) and older ARMv7 edge devices without host C/C++ toolchains.
   - For this initial version, **only MJPEG stream content** is supported (`video/JPEG` over RTP, payload type 26, RFC 2435). MJPEG requires no video decoding or pixel color space conversions—RTP packets assemble directly into standard JPEG byte arrays.
2. **Round-Robin Snapshot Topics with `/pic` and `/meta`**:
   - User configures `slots` (e.g. $N = 5$) and `topicPrefix` (e.g. `cameras/front_gate`).
   - For each slot $k \in [1, N]$, snapshots are published to paired subtopics:
     - **Binary Picture**: `<topicPrefix>/capture/frames/<slot>` (raw JPEG `[]byte`)
     - **JSON Metadata**: `<topicPrefix>/capture/frames/<slot>/meta` (JSON payload containing at least the timestamp)
   - Cycles sequentially: $1 \rightarrow 2 \rightarrow \dots \rightarrow N \rightarrow 1$.
   - Each slot is overwritten with the newest picture and metadata when its turn arrives.
3. **Capture Modes**:
   - **Continuous (`CONTINUOUS`)**: Automatically captures at a configured interval (`intervalMs`, e.g., 1000 ms = 1 fps) and advances the slot.
   - **Triggered (`TRIGGERED`)**: Listens to an MQTT trigger topic (e.g., `<topicPrefix>/trigger`). When a trigger message is received, takes the next frame and writes to the next slot.
   - **Both (`BOTH`)**: Periodic continuous capture, with immediate ad-hoc slot captures whenever the trigger topic is published to.
4. **Companion State / Pointer Topic**:
   - Whenever slot $k$ is written, the bridge also publishes an active pointer JSON to `<topicPrefix>/capture/latest`:
     ```json
     {
       "camera": "front_gate",
       "slot": 1,
       "picTopic": "cameras/front_gate/capture/frames/1",
       "metaTopic": "cameras/front_gate/capture/frames/1/meta",
       "timestamp": "2026-09-16T10:15:30.123Z",
       "timestampMs": 1789553730123,
       "bytes": 142800,
       "trigger": "continuous"
     }
     ```
   - Downstream consumers (HMI panels, Node-RED, AI/vision workers) can subscribe to `<topicPrefix>/capture/latest` or `<topicPrefix>/capture/frames/+/meta` without needing to pull high-bandwidth binary pictures until desired.
   - Every capture also updates `<topicPrefix>/capture/latest/pic` and `<topicPrefix>/capture/latest/meta`. A capture initiated by the MQTT trigger topic additionally updates `<topicPrefix>/capture/snapshot/pic` and `<topicPrefix>/capture/snapshot/meta`.
   - The retained `<topicPrefix>/status` topic reports the camera name, node, connection state, last connection error, and update timestamp.
5. **Dashboard Management**:
   - Full configuration UI in `monster-mq-dashboard`: list view, detail editor with slot preview, connection status, manual "Trigger Snapshot" testing, and live preview.

---

## 2. Library Selection

We will use **`github.com/bluenviron/gortsplib/v4`** and **`github.com/bluenviron/mediacommon`**:
- **Pure Go**: Zero CGO, pure standard library network sockets.
- **Battle-Tested**: The core engine behind MediaMTX (formerly `rtsp-simple-server`).
- **MJPEG Depacketizer**: Includes `rtpmjpeg.Decoder` which reassembles RTP packets directly into complete JPEG files/byte arrays (`[]byte`) with negligible CPU and memory overhead on Raspberry Pi.
- **Transports**: Supports TCP interleaved (default, traverses NATs and firewalls cleanly) and UDP.

---

## 3. Configuration & Data Model

### Feature Flag (`config.yaml`)
```yaml
Features:
  RtspCamera: true   # Toggles the RTSP camera bridge manager
```

### JSON Device Configuration (`DeviceConfigStore`)
Stored with device `type: "RTSP_CAMERA"`:
```json
{
  "url": "rtsp://admin:secret@192.168.1.50:554/mjpeg_stream",
  "transport": "TCP",
  "topicPrefix": "cameras/front_gate",
  "mode": "CONTINUOUS",
  "intervalMs": 1000,
  "slots": 5,
  "triggerTopic": "cameras/front_gate/trigger",
  "retain": true,
  "qos": 0,
  "publishMetadata": true
}
```

---

## 4. GraphQL Schema (`rtspcamera.graphqls`)

File: `internal/graphql/schema/rtspcamera.graphqls`

```graphql
enum RtspTransport {
    TCP
    UDP
}

enum RtspCaptureMode {
    CONTINUOUS
    TRIGGERED
    BOTH
}

type RtspCameraConfig {
    url: String!
    transport: RtspTransport!
    topicPrefix: String!
    mode: RtspCaptureMode!
    intervalMs: Int!
    slots: Int!
    triggerTopic: String
    retain: Boolean!
    qos: Int!
    publishMetadata: Boolean!
}

type RtspCameraMetrics {
    connected: Boolean!
    framesReceived: Float!
    snapshotsPublished: Float!
    currentSlot: Int!
    lastSnapshotAt: String
    lastError: String
    timestamp: String!
}

type RtspCamera {
    name: String!
    nodeId: String!
    enabled: Boolean!
    config: RtspCameraConfig!
    createdAt: String!
    updatedAt: String!
    isOnCurrentNode: Boolean!
    metrics: [RtspCameraMetrics!]!
}

type RtspCameraResult {
    camera: RtspCamera
    success: Boolean!
    message: String
}

input RtspCameraConfigInput {
    url: String!
    transport: RtspTransport = TCP
    topicPrefix: String!
    mode: RtspCaptureMode = CONTINUOUS
    intervalMs: Int = 1000
    slots: Int = 5
    triggerTopic: String
    retain: Boolean = true
    qos: Int = 0
    publishMetadata: Boolean = true
}

input RtspCameraInput {
    name: String!
    nodeId: String
    enabled: Boolean = true
    config: RtspCameraConfigInput!
}

extend type Query {
    rtspCameras(name: String, node: String): [RtspCamera!]!
    rtspCamera(name: String!): RtspCamera
}

extend type Mutation {
    createRtspCamera(input: RtspCameraInput!): RtspCameraResult!
    updateRtspCamera(name: String!, input: RtspCameraInput!): RtspCameraResult!
    deleteRtspCamera(name: String!): Boolean!
    toggleRtspCamera(name: String!, enabled: Boolean!): RtspCameraResult!
    triggerRtspCameraSnapshot(name: String!): RtspCameraResult!
}
```

---

## 5. Go Backend Architecture (`monster-mq-edge`)

### Package: `internal/bridge/rtspcamera/`

1. **`config.go`**:
   - Go structs `Config`, `MetricsSnapshot`.
   - `SnapshotMeta` struct:
     ```go
     type SnapshotMeta struct {
         Camera      string `json:"camera"`
         Slot        int    `json:"slot"`
         Timestamp   string `json:"timestamp"`
         TimestampMs int64  `json:"timestampMs"`
         Bytes       int    `json:"bytes"`
         ContentType string `json:"contentType"`
         Topic       string `json:"topic"`
         Trigger     string `json:"trigger"`
     }
     ```
   - Validation helper: validates URL scheme (`rtsp://`), `slots >= 1`, `intervalMs >= 50`, topic format.

2. **`connector.go`**:
   - Represents an active connection to an RTSP camera stream.
   - Lifecycle:
     - Connects via `gortsplib.Client{}`.
     - Calls `Describe()`, searches SDP for `*format.MJPEG`.
     - *If stream format is not MJPEG*: sets `lastError = "stream format is not MJPEG (H.264/H.265 not supported in pure-Go edge mode)"` and backs off.
     - Sets up RTP track with `rtpmjpeg.Decoder`.
     - Calls `Play()`.
   - Frame Caching:
     - Maintains atomic/mutex pointer to the latest received `[]byte` JPEG frame.
   - Round-Robin Engine:
     - Maintains `currentSlot uint32` (atomic counter, 1-indexed up to `slots`).
     - `publishSnapshot(frame []byte, triggerSource string)`:
       1. Atomically advance slot: `slot = (atomic.AddUint32(&c.currentSlot, 1)-1)%slots + 1`.
       2. Now = `time.Now().UTC()`.
       3. Pic topic = `fmt.Sprintf("%s/capture/frames/%d", c.cfg.TopicPrefix, slot)`.
       4. Meta topic = `fmt.Sprintf("%s/capture/frames/%d/meta", c.cfg.TopicPrefix, slot)`.
       5. Publish binary JPEG payload to pic topic: `Publish(picTopic, frame, c.cfg.Retain, byte(c.cfg.QoS))`.
       6. Prepare and marshal `SnapshotMeta`:
          ```go
          meta := SnapshotMeta{
              Camera:      c.name,
              Slot:        int(slot),
              Timestamp:   now.Format(time.RFC3339Nano),
              TimestampMs: now.UnixMilli(),
              Bytes:       len(frame),
              ContentType: "image/jpeg",
              Topic:       picTopic,
              Trigger:     triggerSource,
          }
          metaBytes, _ := json.Marshal(meta)
          Publish(metaTopic, metaBytes, c.cfg.Retain, byte(c.cfg.QoS))
          ```
       7. If `publishMetadata`: publish active pointer JSON to `<topicPrefix>/capture/latest`:
          ```json
          {
            "camera": "front_gate",
            "slot": 1,
            "picTopic": "cameras/front_gate/capture/frames/1",
            "metaTopic": "cameras/front_gate/capture/frames/1/meta",
            "timestamp": "2026-09-16T10:15:30.123Z",
            "timestampMs": 1789553730123,
            "bytes": 142800,
            "trigger": "continuous"
          }
          ```
   - Continuous Mode:
     - Ticker runs at `time.Duration(cfg.IntervalMs) * time.Millisecond`.
     - Takes cached frame and calls `publishSnapshot(..., "continuous")`.
   - Triggered Mode:
     - Uses `LocalSubscriber` to subscribe to `cfg.TriggerTopic`.
     - On inbound message, immediately captures next frame and calls `publishSnapshot(..., "topic_trigger")`.
   - Auto-reconnect:
     - Backoff loop (1s, 2s, 5s, max 30s) if TCP connection drops.

3. **`manager.go`**:
   - Manages connectors for devices with `Type == "RTSP_CAMERA"`.
   - Integrates with `DeviceConfigStore` and lifecycle methods: `Start()`, `Stop()`, `Reload()`, `Trigger(name)`.

4. **Integration into Broker Server & GraphQL Resolvers**:
   - `internal/broker/server.go`: Initialize `rtspcamera.Manager` when `cfg.Features.RtspCamera` is enabled.
   - `internal/graphql/resolvers/rtspcamera.go`: Implement queries and mutations.

---

## 6. Dashboard Integration (`monster-mq-dashboard`)

### 1. Navigation (`dashboard/src/js/sidebar.js`)
Add to `Bridging` section:
```javascript
{ href: '/pages/rtsp-cameras.html', icon: 'video-camera', text: 'RTSP Cameras', feature: 'RtspCamera' }
```

### 2. List Page (`pages/rtsp-cameras.html` & `js/rtsp-cameras.js`)
- Metrics Grid:
  - Total Cameras (`metric-card`)
  - Active / Connected Cameras (`metric-card is-ok`)
  - Total Snapshots Published (`metric-card`)
  - Local Node Cameras (`metric-card is-info`)
- Table Columns:
  - Status Indicator (Online / Offline / Connecting)
  - Camera Name
  - RTSP URL
  - Mode (`Continuous`, `Triggered`, `Both`)
  - Slots (e.g. `5 slots (1..5)`)
  - Interval (e.g. `1000 ms`)
  - Actions:
    - **Trigger Snap** (Quick test button)
    - Toggle Enabled
    - Edit (navigates to detail page)
    - Delete

### 3. Detail Page (`pages/rtsp-camera-detail.html` & `js/rtsp-camera-detail.js`)
Follows `dashboard/DESIGN.md`:
- Breadcrumbs: `RTSP Cameras › [Camera Name]`
- Header Actions: Trigger Snapshot, Delete (destructive), Save (primary).
- **Section 1: Connection Settings**:
  - Name, Node ID, Enabled toggle.
  - RTSP URL (e.g. `rtsp://user:pass@192.168.1.50:554/stream`).
  - Transport (`TCP` / `UDP`).
- **Section 2: Capture & Round-Robin Settings**:
  - Topic Prefix (e.g. `cameras/front_gate`).
  - Live Topic Preview pills showing:
    - `.../capture/frames/1` & `.../capture/frames/1/meta`
    - `.../capture/frames/2` & `.../capture/frames/2/meta`
    - `...`
    - `.../capture/latest` (Pointer)
  - Capture Mode (`Continuous`, `Triggered`, `Both`).
  - Number of Slots (`slots`, number input 1–100).
  - Interval (`intervalMs`, number input).
  - Trigger Topic (`triggerTopic`, text input).
  - Retain & QoS options.
  - Publish Latest Pointer toggle.
- **Section 3: Live Preview & Status**:
  - Displays current active slot number.
  - Image preview card showing the most recent snapshot published to the broker and its accompanying JSON metadata.

---

## 7. Verification & Testing

1. **Unit & Connector Tests** (`internal/bridge/rtspcamera/`):
   - Test round-robin slot calculation and wrap-around ($1 \rightarrow N \rightarrow 1$).
   - Test synchronous publishing of `<slot>/pic` and `<slot>/meta` (with valid timestamp).
   - Test trigger topic subscriber triggering snapshot publishing.
2. **Mock RTSP Stream Integration Test** (`test/integration/rtsp_test.go`):
   - Run a lightweight in-memory RTSP MJPEG server feeding synthetic MJPEG RTP packets.
   - Verify broker publishes binary JPEG payloads to `cameras/test/capture/frames/1`.
   - Verify broker publishes JSON metadata to `cameras/test/capture/frames/1/meta` containing timestamp.
   - Verify `cameras/test/capture/latest` receives updated slot pointers.
3. **Build & Lint Verification**:
   - Run `make lint` and `make build` (ensuring `CGO_ENABLED=0` succeeds).
   - In `dashboard/`: `npm run build` to verify Vite bundle outputs cleanly without errors.
