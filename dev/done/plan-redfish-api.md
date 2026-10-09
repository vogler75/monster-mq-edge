# Plan: DMTF Redfish API for MonsterMQ Edge

This document defines the architecture, design rationale, DMTF Redfish REST API contracts, MQTT payload extraction using JSON Schema, `LastValue` memory archive integration with configurable topic prefixes, configuration model, and implementation plan for adding a native **Redfish API** to **MonsterMQ Edge**.

---

## 1. Overview & Core Objectives

[Redfish](https://www.dmtf.org/standards/redfish) (DMTF DSP0266 standard) is an open industry standard specification for managing data centers, servers, industrial equipment, edge nodes, and IoT hardware using modern hypermedia RESTful APIs formatted in JSON (following OData v4 conventions).

In industrial IoT, server rooms, and edge infrastructure, monitoring systems (e.g. Data Center Infrastructure Management [DCIM], Prometheus Redfish exporter, Nagios, Zabbix, BMC managers, and edge supervisory software) query Redfish endpoints to inspect physical enclosures, telemetry, environmental sensors (temperatures, power, voltages, humidity, pressure, fan speeds), and compute nodes.

### Core Objectives
1. **DMTF Redfish Standard REST Endpoints**: Expose standard hypermedia JSON endpoints under `/redfish/v1/` (`ServiceRoot`, `Chassis`, `Sensors`, `Thermal`, `Power`, `Systems`, `TelemetryService`, `Managers`, and `EventService`).
2. **MQTT Telemetry Ingestion**: Make selected MQTT topic data available as Redfish sensor and metric resources.
3. **JSON Schema & JSONPath Payload Extraction**: Define payload schemas with field-level JSONPath mappings and array expansion (`arrayPath`), matching the proven schema model used by MonsterMQ data loggers.
4. **LastValue Memory Archive as Sensor Registry**: Store normalized Redfish sensor and device states in the broker's **`LastValue` memory store** under **configurable topic paths** (e.g. `{topicPrefix}/{chassisId}/sensors/{sensorId}`). No `$SYS`-style or hardcoded `$redfish` topics.
5. **DeviceConfigStore for Dynamic Configuration**: Manage all Redfish data gateways exclusively via `DeviceConfigStore` (device type: `"Redfish"`) and the GraphQL API.
6. **Zero CGO**: Pure Go implementation cross-compilable for ARM64/ARMv7 (Raspberry Pi 4/5).
7. **Parity Compliance**: Maintain strict GraphQL schema and storage parity with the JVM broker.

---

## 2. Redfish Standard Resource Hierarchy

The Redfish service will expose standard DMTF-compliant endpoints under `/redfish/v1/`:

```
GET /redfish/v1/
├── GET /redfish/v1/odata                       # OData Service Document
├── GET /redfish/v1/$metadata                   # CSDL Metadata document
├── GET /redfish/v1/Chassis                     # ChassisCollection
│   └── GET /redfish/v1/Chassis/{chassisId}     # Chassis (Enclosure / Zone / Edge Box)
│       ├── GET .../Sensors                     # SensorCollection (Modern Redfish v1.6+)
│       │   └── GET .../Sensors/{sensorId}      # Sensor (Reading, Units, Status, Thresholds)
│       ├── GET .../Thermal                     # Thermal (Legacy Temperatures & Fans)
│       └── GET .../Power                       # Power (Legacy Voltages & PowerControl)
├── GET /redfish/v1/Systems                     # ComputerSystemCollection
│   └── GET /redfish/v1/Systems/{systemId}      # ComputerSystem (Edge Node Compute Info)
├── GET /redfish/v1/Managers                    # ManagerCollection
│   └── GET /redfish/v1/Managers/{managerId}    # Manager (MonsterMQ Edge Daemon)
├── GET /redfish/v1/TelemetryService            # TelemetryService
│   ├── GET .../MetricReports                   # MetricReportCollection
│   └── GET .../MetricReports/{reportId}        # MetricReport (Aggregated sensor readings)
├── GET /redfish/v1/EventService                # EventService (SSE / Webhooks)
│   ├── GET .../Subscriptions                   # Event Subscriptions
│   └── GET .../SSE                             # Server-Sent Events stream for threshold alerts
└── GET /redfish/v1/JsonSchemas                 # JsonSchemaFileCollection
    └── GET .../JsonSchemas/{schemaId}          # DMTF JSON Schema definitions
```

---

## 3. MQTT Payload Extraction via JSON Schema & JSONPath

Following the established data logger schema model in MonsterMQ, each Redfish gateway configuration defines how incoming MQTT messages are validated, parsed, and mapped into Redfish resources.

### 3.1 JSON Schema Mapping Structure

```json
{
  "type": "object",
  "properties": {
    "reading": { "type": "number" },
    "sensorId": { "type": "string" },
    "chassisId": { "type": "string" },
    "name": { "type": "string" },
    "readingType": { "type": "string" },
    "readingUnits": { "type": "string" },
    "status": { "type": "string" },
    "health": { "type": "string" },
    "ts": { "type": "string", "format": "timestamp" }
  },
  "required": ["reading"],
  "mapping": {
    "reading": "$.metrics.temperature",
    "sensorId": "$.sensor.id",
    "chassisId": "$.location.rack",
    "name": "$.sensor.label",
    "readingType": "$.sensor.type",
    "readingUnits": "$.sensor.unit",
    "health": "$.diagnostics.health",
    "ts": "$.timestamp"
  },
  "arrayPath": "$.sensors[*]"
}
```

### 3.2 Key Extraction Capabilities
1. **Direct Value Extraction (`mapping`)**: JSONPath expressions (e.g. `$.metrics.temperature`, `$.power.watts`, `$.ambient.humidity`) extract fields from nested payloads.
2. **Array Expansion (`arrayPath`)**: If an MQTT message contains a list of readings (e.g. `{"sensors": [{"id": "s1", "val": 22.4}, {"id": "s2", "val": 60.1}]}`), `arrayPath: "$.sensors[*]"` unrolls each element into an independent Redfish sensor instance.
3. **Dynamic vs. Static Fallbacks**:
   - **`sensorId`**: Extracted from payload via JSONPath, OR extracted from MQTT topic wildcard token (e.g. `sensors/{chassisId}/{sensorId}`), OR statically configured.
   - **`chassisId`**: Extracted from payload, topic wildcard, or falls back to mapping default (e.g. `"EdgeNode"`).
   - **`readingType`**: Redfish standard enum (`Temperature`, `Humidity`, `Voltage`, `Current`, `Power`, `EnergykWh`, `Pressure`, `LiquidFlow`, `Frequency`, `Percent`, `AirFlow`). If omitted in payload, uses mapping default.
   - **`readingUnits`**: DMTF unit string (`Cel`, `V`, `A`, `W`, `kW`, `kWh`, `Pa`, `Bar`, `RPM`, `%`, `Hz`). If omitted in payload, uses mapping default.
4. **Thresholds & Automatic Health Calculation**:
   - Mappings configure threshold limits (`upperCaution`, `upperCritical`, `lowerCaution`, `lowerCritical`).
   - Health state is calculated automatically:
     - `reading >= upperCritical || reading <= lowerCritical` $\rightarrow$ `Status.Health = "Critical"`
     - `reading >= upperCaution || reading <= lowerCaution` $\rightarrow$ `Status.Health = "Warning"`
     - Otherwise $\rightarrow$ `Status.Health = "OK"`
5. **Timestamp Handling**:
   - `"format": "timestamp"`: parses ISO 8601 strings or epoch ms. Defaults to message arrival time if missing.

---

## 4. Architecture: Configurable Topic Prefix on LastValue Memory Store

Instead of hardcoding a `$redfish` or `$SYS` prefix, each Redfish Gateway configures its own **`topicPrefix`** (e.g. `"redfish"`, `"telemetry/redfish"`, `"nodes/{NodeId}/redfish"`, or `"sensors"`).

```
                                  ┌───────────────────────────────┐
                                  │       MQTT Publish Hook       │
                                  └───────────────┬───────────────┘
                                                  │ (pubsub.Bus)
                                                  ▼
                                  ┌───────────────────────────────┐
                                  │      Redfish Ingestion        │
                                  │  - JSON Schema validation     │
                                  │  - Pure Go JSONPath Mapper    │
                                  │  - Array unrolling            │
                                  │  - Threshold health check     │
                                  └───────────────┬───────────────┘
                                                  │
                                                  │ Write normalized state to
                                                  │ {topicPrefix}/{chassis}/sensors/*
                                                  ▼
                                  ┌───────────────────────────────┐
                                  │    LastValue Memory Store     │
                                  │   (stores.MessageStore)       │
                                  └───────────────┬───────────────┘
                                                  │
                                                  │ Query matching topics
                                                  ▼
┌──────────────────────────┐      ┌───────────────────────────────┐
│ Redfish REST API Clients │ ───▶ │      Redfish HTTP Router      │
│ (DCIM, Exporters, Zabbix)│      │    /redfish/v1/Chassis/...    │
│                          │ ◀─── │    /redfish/v1/Sensors/...    │
└──────────────────────────┘      └───────────────────────────────┘
```

### 4.1 Configurable Topic Structure

| Configurable Topic Path | Content / Schema | Redfish Endpoint Destination |
|:---|:---|:---|
| `{topicPrefix}/{chassisId}/sensors/{sensorId}` | Normalized Sensor JSON (`reading`, `type`, `unit`, `health`, `thresholds`, `timestamp`, `sourceTopic`) | `GET /redfish/v1/Chassis/{chassisId}/Sensors/{sensorId}` |
| `{topicPrefix}/{chassisId}/thermal/{sensorId}` | Temperature readings & fan stats | `GET /redfish/v1/Chassis/{chassisId}/Thermal` |
| `{topicPrefix}/{chassisId}/power/{sensorId}` | Voltage, Current, Power readings | `GET /redfish/v1/Chassis/{chassisId}/Power` |
| `{topicPrefix}/systems/{systemId}` | System compute status & metadata | `GET /redfish/v1/Systems/{systemId}` |
| `{topicPrefix}/telemetry/{reportId}` | Aggregated metric report | `GET /redfish/v1/TelemetryService/MetricReports/{reportId}` |

### 4.2 Benefits
1. **Configurable & Transparent**: Users decide where normalized Redfish state lives in the topic namespace (e.g. `redfish/rack1/sensors/temp1` or `datacenter/redfish/...`).
2. **Zero Hardcoded Namespaces**: No collision with `$SYS` or special broker prefix rules.
3. **Multi-Gateway Support**: Different Redfish gateways can use distinct topic prefixes (e.g. `redfish/buildingA`, `redfish/buildingB`).
4. **Direct MQTT Subscriptions**: Clients on the MQTT bus can subscribe directly to `{topicPrefix}/#` to receive normalized sensor data.

---

## 5. Package Layout & Components (`internal/redfish/`)

- **`manager.go`**: Central coordinator. Watches `DeviceConfigStore` for `"Redfish"` device configurations, reloads gateways dynamically, initializes the HTTP listener, and manages lifecycle.
- **`subscriber.go`**: Subscribes to configured MQTT topic filters via `pubsub.Bus`, invokes the mapper, computes health states, and stores normalized messages into `LastValue()` under `{topicPrefix}/{chassisId}/sensors/{sensorId}`.
- **`mapper.go`**: Pure Go JSONPath evaluator and array unroller for extracting sensor fields according to `jsonSchema`.
- **`server.go`**: HTTP router serving standard DMTF Redfish JSON responses by querying the `LastValue()` store using the configured `topicPrefix`.
- **`models.go`**: DMTF OData v4 JSON models (`ServiceRoot`, `Chassis`, `Sensor`, `Thermal`, `Power`, `ComputerSystem`, `MetricReport`, `EventService`).

---

## 6. Configuration Model

### 6.1 Server Configuration (`config.yaml`)

Static configuration defines broker-level listener settings and feature gating:

```yaml
Features:
  Redfish: true

Redfish:
  Enabled: true
  Port: 8000                  # Dedicated port (e.g. 8000), or 0 to multiplex on main GraphQL port (:4000)
  MountPath: "/redfish/v1"
  DefaultChassisId: "EdgeNode"
  DefaultSystemId: "edge-node"
  DefaultManagerId: "monstermq-edge"
  AnonymousEnabled: true      # Allow unauthenticated Redfish GET requests (or require Basic Auth)
```

### 6.2 Data Gateways in `DeviceConfigStore` (Type: `"Redfish"`)

All data source gateways are stored dynamically as device configurations in `DeviceConfigStore` (`deviceconfigs` table) and managed via GraphQL / Web Dashboard:

- **`name`**: Gateway identifier (e.g. `"EnvironmentSensors"`).
- **`type`**: `"Redfish"`.
- **`nodeId`**: Cluster node handling the mapping (defaults to current node).
- **`enabled`**: `true` / `false`.
- **`config`**: JSON object containing:
  ```json
  {
    "topicPrefix": "redfish",
    "topicFilters": ["sensors/+/environment"],
    "chassisId": "EdgeNode",
    "defaultReadingType": "Temperature",
    "defaultReadingUnits": "Cel",
    "thresholds": {
      "upperCaution": 70.0,
      "upperCritical": 85.0,
      "lowerCaution": 0.0,
      "lowerCritical": -10.0
    },
    "jsonSchema": {
      "type": "object",
      "properties": {
        "sensorId": { "type": "string" },
        "reading": { "type": "number" },
        "readingType": { "type": "string" },
        "readingUnits": { "type": "string" },
        "ts": { "type": "string", "format": "timestamp" }
      },
      "required": ["reading"],
      "mapping": {
        "sensorId": "$.sensor_id",
        "reading": "$.temperature",
        "readingType": "$.type",
        "readingUnits": "$.unit",
        "ts": "$.timestamp"
      },
      "arrayPath": "$.sensors[*]"
    }
  }
  ```

---

## 7. GraphQL Schema & API Parity (`schema-redfish.graphqls`)

```graphql
type RedfishThresholds {
    upperCaution: Float
    upperCritical: Float
    lowerCaution: Float
    lowerCritical: Float
}

type RedfishMappingConfig {
    topicPrefix: String!
    topicFilters: [String!]!
    chassisId: String
    defaultReadingType: String
    defaultReadingUnits: String
    thresholds: RedfishThresholds
    jsonSchema: JSON!
}

type RedfishMapping {
    name: String!
    nodeId: String!
    enabled: Boolean!
    config: RedfishMappingConfig!
    createdAt: String!
    updatedAt: String!
    isOnCurrentNode: Boolean!
}

type RedfishSensorStatus {
    id: String!
    name: String!
    chassisId: String!
    topic: String!
    reading: Float!
    readingType: String!
    readingUnits: String!
    health: String!
    state: String!
    lastUpdated: String!
}

input RedfishThresholdsInput {
    upperCaution: Float
    upperCritical: Float
    lowerCaution: Float
    lowerCritical: Float
}

input RedfishMappingConfigInput {
    topicPrefix: String
    topicFilters: [String!]!
    chassisId: String
    defaultReadingType: String
    defaultReadingUnits: String
    thresholds: RedfishThresholdsInput
    jsonSchema: JSON!
}

extend type Query {
    redfishMappings: [RedfishMapping!]!
    redfishMapping(name: String!): RedfishMapping
    redfishLiveSensors(chassisId: String): [RedfishSensorStatus!]!
}

extend type Mutation {
    saveRedfishMapping(name: String!, config: RedfishMappingConfigInput!, enabled: Boolean): RedfishMapping!
    deleteRedfishMapping(name: String!): Boolean!
    toggleRedfishMapping(name: String!, enabled: Boolean!): RedfishMapping!
}
```

---

## 8. Sample DMTF Redfish API Responses

### 8.1 Service Root (`GET /redfish/v1/`)
```json
{
  "@odata.context": "/redfish/v1/$metadata#ServiceRoot.ServiceRoot",
  "@odata.id": "/redfish/v1",
  "@odata.type": "#ServiceRoot.v1_15_0.ServiceRoot",
  "Id": "RootService",
  "Name": "MonsterMQ Edge Redfish Service",
  "RedfishVersion": "1.18.0",
  "UUID": "550e8400-e29b-41d4-a716-446655440000",
  "Chassis": { "@odata.id": "/redfish/v1/Chassis" },
  "Systems": { "@odata.id": "/redfish/v1/Systems" },
  "Managers": { "@odata.id": "/redfish/v1/Managers" },
  "TelemetryService": { "@odata.id": "/redfish/v1/TelemetryService" },
  "EventService": { "@odata.id": "/redfish/v1/EventService" },
  "JsonSchemas": { "@odata.id": "/redfish/v1/JsonSchemas" }
}
```

### 8.2 Sensor Resource (`GET /redfish/v1/Chassis/EdgeNode/Sensors/temp-cpu`)
```json
{
  "@odata.context": "/redfish/v1/$metadata#Sensor.Sensor",
  "@odata.id": "/redfish/v1/Chassis/EdgeNode/Sensors/temp-cpu",
  "@odata.type": "#Sensor.v1_7_0.Sensor",
  "Id": "temp-cpu",
  "Name": "CPU Temperature",
  "Reading": 48.2,
  "ReadingType": "Temperature",
  "ReadingUnits": "Cel",
  "ReadingRangeMin": -20.0,
  "ReadingRangeMax": 100.0,
  "Status": {
    "State": "Enabled",
    "Health": "OK"
  },
  "Thresholds": {
    "UpperCaution": { "Reading": 70.0 },
    "UpperCritical": { "Reading": 85.0 }
  },
  "Oem": {
    "MonsterMQ": {
      "Topic": "factory/edge/host",
      "NormalizedTopic": "redfish/EdgeNode/sensors/temp-cpu",
      "LastUpdated": "2026-08-20T12:00:00Z"
    }
  }
}
```

---

## 9. Implementation Steps & Milestones

1. **Step 1: Pure Go JSON Schema & JSONPath Payload Mapper (`internal/redfish/mapper.go`)**
   - Implement fast, zero-CGO JSONPath field extraction and array unrolling.
   - Unit tests covering flat, nested, and array-based MQTT JSON payloads.
2. **Step 2: Pub/Sub Ingestion & LastValue Store Integration (`internal/redfish/subscriber.go`)**
   - Wire subscriber to `pubsub.Bus`.
   - Normalize incoming sensor values, evaluate health/thresholds, and persist under `{topicPrefix}/{chassisId}/sensors/{sensorId}` in `LastValue()` store.
3. **Step 3: Redfish REST API Server (`internal/redfish/server.go`)**
   - Implement handlers for ServiceRoot, Chassis, Sensors, Thermal, Power, Systems, TelemetryService, and EventService SSE.
   - Handlers query `{topicPrefix}/...` topics directly from `LastValue()`.
4. **Step 4: DeviceConfigStore & Lifecycle Manager (`internal/redfish/manager.go`)**
   - Load and watch `"Redfish"` device configs from `DeviceConfigStore`.
   - Update topic subscriptions dynamically on config changes.
5. **Step 5: GraphQL Schema & Resolvers**
   - Add `schema-redfish.graphqls` under `internal/graphql/schema/`.
   - Implement queries and mutations in resolvers and wire into `internal/broker/server.go`.
6. **Step 6: Integration Testing & DMTF Validation**
   - Write end-to-end black-box tests in `test/integration/redfish_test.go` verifying MQTT publish $\rightarrow$ LastValue `{topicPrefix}/...` update $\rightarrow$ Redfish REST GET.
   - Validate with curl and Redfish tools.

---

## 10. Verification Plan

### Automated Tests
- `go test ./internal/redfish/...` — unit tests for JSONPath mapping, array unrolling, threshold calculation, and OData formatting.
- `go test ./test/integration -run TestRedfish` — end-to-end black-box tests:
  - Redfish ServiceRoot discovery (`/redfish/v1`).
  - MQTT publish $\rightarrow$ JSON Schema mapping $\rightarrow$ `{topicPrefix}/...` written in LastValue store $\rightarrow$ retrieved via `/redfish/v1/Chassis/{id}/Sensors/{id}`.
  - Array unrolling (`arrayPath`) creating multiple sensor resources.
  - Threshold triggers updating `Status.Health` between `OK`, `Warning`, and `Critical`.
  - Legacy `/Thermal` and `/Power` endpoints correctly grouping sensors.
- `make lint` / `go vet ./...` — zero lint warnings, zero CGO dependencies.
