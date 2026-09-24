# Plan: Raspberry Pi GPIO Pin Bridge for Edge Broker

> Superseded design direction (2026-09-23): see
> [Local I/O for IOT2050 and Raspberry Pi](plan-iot2050-local-io.md).
> Use an owned Linux GPIO v2 backend with reusable board/shield profiles,
> without `warthog618/gpiod` or `go-gpiocdev`. The GraphQL extension and
> mock-only test approach below are historical proposals, not approved work.
> Raspberry Pi 4 and Pi 5 GPIO support is required in the shared plan's first
> release, with board profiles, dedicated LocalIO GraphQL management and
> physical hardware acceptance tests. The historical design below is not a
> separate implementation path.

## Summary

The `monster-mq-edge` broker is designed to run on resource-constrained devices, such as the Raspberry Pi, and interface directly with local equipment and sensors. While the main Java/Kotlin broker has extensive device bridging capabilities (like PLC4X, OPC UA, etc.), edge deployments often need direct hardware integration via the Raspberry Pi's GPIO pins.

This plan details how to implement a native **Raspberry Pi GPIO Pin Bridge** in the Go-based edge broker.

Key constraints:
- **Pure Go (Zero CGO)**: Cross-compilation to Linux/ARM (32-bit and 64-bit) must remain simple, fast, and not require external toolchains.
- **Pi 5 Compatibility**: Traditionally, GPIO libraries in Go (e.g., `go-rpio`) used direct memory mapping (`/dev/gpiomem`). The Raspberry Pi 5 introduced a new I/O controller (the RP1 chip), rendering memory-mapped registers incompatible. The solution must use the Linux standard character device interface (`/dev/gpiochip*`).

---

## Library Selection

We will use the **[`github.com/warthog618/gpiod`](https://github.com/warthog618/gpiod)** library:
1. **Pure Go**: Implements `ioctl` calls on `/dev/gpiochip*` file descriptors natively in Go without CGO.
2. **Modern Linux Standards**: Targets the Linux character device GPIO API, which works out-of-the-box on Raspberry Pi 3, 4, and 5 under modern kernels.
3. **Edge Triggering**: Supports kernel-level interrupts for detecting state changes (Rising, Falling, Both edges) efficiently without busy-polling.

---

## Configuration Schema

We will add a new device type: `"RpiGpio-Client"`. Its configurations will be stored in the existing `DeviceConfigStore` (with a JSON configuration representation) and toggled via the `Features.RpiGpio` configuration flag.

### `config.yaml` Feature Flag
```yaml
Features:
  RpiGpio: false  # Toggles the feature verticle/manager at startup
```

### JSON Device Configuration Structure
```json
{
  "chip": "/dev/gpiochip0",
  "pins": [
    {
      "pinNumber": 17,
      "direction": "IN",
      "topic": "rpi/gpio17",
      "edge": "BOTH",
      "pull": "UP",
      "debounceMs": 50,
      "publishOnChange": true,
      "qos": 1,
      "retained": false
    },
    {
      "pinNumber": 18,
      "direction": "OUT",
      "topic": "rpi/gpio18/set",
      "initialState": "LOW",
      "payloadOn": "ON",
      "payloadOff": "OFF"
    }
  ]
}
```

---

## GraphQL Schema (`rpigpio.graphqls`)

We will define a new GraphQL schema file under `internal/graphql/schema/rpigpio.graphqls`:

```graphql
enum RpiGpioDirection {
    IN
    OUT
}

enum RpiGpioEdge {
    NONE
    RISING
    FALLING
    BOTH
}

enum RpiGpioPull {
    NONE
    UP
    DOWN
}

enum RpiGpioState {
    LOW
    HIGH
}

type RpiGpioPin {
    pinNumber: Int!
    direction: RpiGpioDirection!
    topic: String!
    edge: RpiGpioEdge!
    pull: RpiGpioPull!
    debounceMs: Int!
    publishOnChange: Boolean!
    qos: Int!
    retained: Boolean!
    initialState: RpiGpioState!
    payloadOn: String!
    payloadOff: String!
}

type RpiGpioConnectionConfig {
    chip: String!
    pins: [RpiGpioPin!]!
}

type RpiGpioClientMetrics {
    messagesIn: Float!
    messagesOut: Float!
    connected: Boolean!
    timestamp: String!
}

type RpiGpioClient {
    name: String!
    namespace: String!
    nodeId: String!
    config: RpiGpioConnectionConfig!
    enabled: Boolean!
    createdAt: String!
    updatedAt: String!
    isOnCurrentNode: Boolean!
    metrics: [RpiGpioClientMetrics!]!
    metricsHistory(from: String, to: String, lastMinutes: Int): [RpiGpioClientMetrics!]!
}

extend type Query {
    rpiGpioClients(name: String, node: String): [RpiGpioClient!]!
}

input RpiGpioPinInput {
    pinNumber: Int!
    direction: RpiGpioDirection!
    topic: String!
    edge: RpiGpioEdge = BOTH
    pull: RpiGpioPull = NONE
    debounceMs: Int = 0
    publishOnChange: Boolean = true
    qos: Int = 0
    retained: Boolean = false
    initialState: RpiGpioState = LOW
    payloadOn: String = "1"
    payloadOff: String = "0"
}

input RpiGpioConnectionConfigInput {
    chip: String = "/dev/gpiochip0"
    pins: [RpiGpioPinInput!]!
}

input RpiGpioClientInput {
    name: String!
    namespace: String!
    nodeId: String!
    enabled: Boolean = true
    config: RpiGpioConnectionConfigInput!
}

type RpiGpioClientResult {
    success: Boolean!
    client: RpiGpioClient
    errors: [String!]!
}

type RpiGpioDeviceMutations {
    create(input: RpiGpioClientInput!): RpiGpioClientResult!
    update(name: String!, input: RpiGpioClientInput!): RpiGpioClientResult!
    delete(name: String!): Boolean!
    start(name: String!): RpiGpioClientResult!
    stop(name: String!): RpiGpioClientResult!
    toggle(name: String!, enabled: Boolean!): RpiGpioClientResult!
    reassign(name: String!, nodeId: String!): RpiGpioClientResult!
    addPin(deviceName: String!, input: RpiGpioPinInput!): RpiGpioClientResult!
    updatePin(deviceName: String!, pinNumber: Int!, input: RpiGpioPinInput!): RpiGpioClientResult!
    deletePin(deviceName: String!, pinNumber: Int!): RpiGpioClientResult!
}

extend type Mutation {
    rpiGpioDevice: RpiGpioDeviceMutations!
}
```

---

## Architecture & Implementation

We will organize the code under a new internal package: `internal/bridge/rpigpio/`.

### 1. Configuration Parsing (`config.go`)
- Defines the Go counterparts of the GraphQL structs.
- Parses configurations from JSON string fields in the database.
- Implements validations: e.g., pin number ranges, duplicate pins, non-empty chip and topics.

### 2. GPIO Connector (`connector.go`)
- Represents a single active GPIO device (e.g. one `gpiochip` chip controller).
- Interacts with `github.com/warthog618/gpiod`.
- **For Input Pins**:
  - Configures the line using `gpiod.WithPull(...)` and `gpiod.WithDebounce(...)`.
  - Sets up an event listener using `gpiod.LineEvent` or a watcher channel.
  - Spawns a background goroutine to read pin level transitions.
  - Formats state to the mapped payloads (`payloadOn` / `payloadOff`) and publishes to the local MQTT broker using the provided publisher.
- **For Output Pins**:
  - Subscribes to the local MQTT bus for the configured topics.
  - On incoming MQTT publishes matching the topic:
    - Sets pin level to `1` if the payload matches `payloadOn`.
    - Sets pin level to `0` if the payload matches `payloadOff`.

### 3. GPIO Manager (`manager.go`)
- Reconciles configuration changes (reload, stop, start) in response to GraphQL mutations.
- Instantiates connectors and watches their execution.

### 4. Stubbing for Non-Linux Environments (`connector_stub.go` / `connector_linux.go`)
- To ensure local compilation on macOS and Windows, we will use Go build tags:
  - `connector_linux.go`: Real implementation importing `gpiod`.
  - `connector_stub.go`: Mock implementation enabling development and testing on desktop machines without physical GPIO lines or linux dependencies.

---

## Verification Plan

### 1. Compilation
- Build for target OS: `GOOS=linux GOARCH=arm64 go build ./...`
- Verify pure Go dependencies: ensure no external C/C++ cross-compilers are needed.

### 2. Stub Testing
- Run test suites on developer machines: `go test ./internal/bridge/rpigpio/...` using mock pins.

### 3. Integration Tests
- Build integration tests that simulate hardware pin changes and verify MQTT topic output:
  - Set up an input pin in mock mode.
  - Simulate a rising edge trigger.
  - Assert that an MQTT message with the configured payload (e.g., `"1"`) is published to the target topic.
  - Publish to an output pin topic and assert that the physical state changes.
