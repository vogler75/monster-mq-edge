# Device Configuration Export/Import GraphQL Plan

This document records the generic GraphQL API used by the dashboard device
configuration export/import page and how it maps to the current broker
implementation.

## Current Status

Implemented.

The active dashboard page is:

- `dashboard/src/pages/device-config-export-import.html`
- `dashboard/src/js/device-config-export-import.js`

The active GraphQL operations are:

- `Query.getDevices(names: [String!])`
- `Mutation.importDevices(configs: [DeviceInput!]!)`

The older dashboard script `dashboard/src/js/device-export-import.js` still
references legacy operation names, but those aliases are not implemented in the
broker schema.

## Implemented GraphQL Schema

The schema is split across:

- `broker/src/main/resources/schema-queries.graphqls`
- `broker/src/main/resources/schema-mutations.graphqls`

```graphql
scalar JSON

type Device {
  name: String!
  namespace: String!
  nodeId: String!
  config: JSON
  enabled: Boolean!
  type: String!
  createdAt: String
  updatedAt: String
}

input DeviceInput {
  name: String!
  namespace: String!
  nodeId: String!
  config: JSON
  enabled: Boolean
  type: String
  createdAt: String
  updatedAt: String
}

type ImportDeviceConfigResult {
  success: Boolean!
  imported: Int!
  failed: Int!
  total: Int!
  errors: [String!]!
}

type Query {
  getDevices(names: [String!]): [Device!]!
}

type Mutation {
  importDevices(configs: [DeviceInput!]!): ImportDeviceConfigResult!
}
```

## Dashboard Queries

The export/import page first loads a lightweight list of devices:

```graphql
query {
  getDevices {
    name
    namespace
    nodeId
    enabled
    type
  }
}
```

When exporting selected devices, the page requests full configuration payloads:

```graphql
query {
  getDevices(names: ["device-a", "device-b"]) {
    name
    namespace
    nodeId
    config
    enabled
    type
    createdAt
    updatedAt
  }
}
```

When importing, the page sends selected exported objects back as `DeviceInput`
values:

```graphql
mutation ImportDevices($configs: [DeviceInput!]!) {
  importDevices(configs: $configs) {
    success
    imported
    failed
    total
    errors
  }
}
```

## Resolver Behavior

### `getDevices(names: [String!])`

Implemented in `QueryResolver.getDevices()`.

Behavior:

- Returns `emptyList()` when `Features.DeviceImportExport` is disabled.
- Fails when the device config store is unavailable.
- Delegates directly to `IDeviceConfigStore.exportConfigs(names)`.
- Returns every stored device when `names` is omitted or empty.
- Returns only matching device names when `names` is provided.

Returned fields:

- `name`: stored device name.
- `namespace`: stored device namespace.
- `nodeId`: stored cluster node assignment.
- `config`: native stored device-specific JSON object.
- `enabled`: stored enabled state.
- `type`: stored device type string.
- `createdAt` and `updatedAt`: stored timestamps converted to strings.

### `importDevices(configs: [DeviceInput!]!)`

Implemented in `MutationResolver.importDevices()`.

Behavior:

- Returns a failed result when `Features.DeviceImportExport` is disabled.
- Requires admin authorization through `GraphQLAuthContext`.
- Fails when the device config store is unavailable.
- Rejects a missing or empty `configs` list.
- Forces `enabled=false` for every imported device before writing to the store.
- Delegates directly to `IDeviceConfigStore.importConfigs(configs)`.
- Sets `total` in the GraphQL result from the submitted list size.
- Sets `success` to `true` only when at least one config was imported.

The store imports each submitted config independently and continues after
per-item failures.

Required input fields in the implemented stores:

- `name`: non-blank string.
- `namespace`: non-blank string.
- `nodeId`: non-blank string.
- `config`: JSON object.

Optional input fields:

- `type`: defaults to `OPCUA-Client` when missing.
- `enabled`: defaults to `true` in the store layer, but GraphQL and file-based
  imports currently override it to `false`.
- `createdAt` and `updatedAt`: ignored; import writes current timestamps.

## Store Implementation

The generic API is implemented at the `IDeviceConfigStore` level:

- `exportConfigs(names: List<String>? = null): Future<List<Map<String, Any?>>>`
- `importConfigs(configs: List<Map<String, Any?>>): Future<ImportDeviceConfigResult>`

Implemented backends:

- PostgreSQL: upsert by `name` with `ON CONFLICT (name) DO UPDATE`.
- CrateDB: upsert by `name` with `ON CONFLICT (name) DO UPDATE`.
- SQLite: `INSERT OR REPLACE` by `name`.
- MongoDB: insert or update document by `name`.

Important implementation detail: import currently writes raw `DeviceConfig`
records through the store implementations. It does not dispatch by `type` to
the device-specific GraphQL create/update resolvers.

## Device Type Strings

Exported and imported `type` values are the stable strings defined in
`DeviceConfig.kt`, for example:

```text
OPCUA-Client
OPCUA-Server
MQTT-Client
KAFKA-Client
WinCCOA-Client
WinCCUA-Client
PLC4X-Client
Neo4j-Client
NATS-Client
Telegram-Client
JDBC-Logger
InfluxDB-Logger
TimeBase-Logger
SparkplugB-Decoder
Redis-Client
Flow-Class
Flow-Object
TopicSchema-Policy
TopicNamespace
Agent
GenAiProvider
MCP-Server
```

Use these stored type values as the compatibility contract for exported files.
Do not use enum-like names such as `OPCUA`, `PLC4X`, or `KAFKA` unless a
translation layer is added.

## Feature Flag Integration

The import/export feature is controlled by `DeviceImportExport`.

Implemented locations:

- `broker/src/main/kotlin/Features.kt`
- `broker/yaml-json-schema.json`
- `QueryResolver.getDevices()`
- `MutationResolver.importDevices()`
- `dashboard/src/js/sidebar.js`

The dashboard sidebar entry is hidden when `Broker.enabledFeatures` does not
include `DeviceImportExport`.

## Differences From The Earlier Plan

The earlier plan said another broker must implement a proposed target API. The
repository now has a concrete implementation with these differences:

- The result type is named `ImportDeviceConfigResult`, not
  `ImportDevicesResult`.
- `Device.namespace`, `Device.nodeId`, and `Device.type` are non-null in the
  implemented schema.
- `DeviceInput.type` is optional in the implemented schema and defaults to
  `OPCUA-Client` in the store layer.
- The implemented API is feature-gated by `DeviceImportExport`.
- `importDevices` requires admin authorization.
- GraphQL and file imports force every imported device to `enabled=false` for
  safety.
- Import does not dispatch through device-specific create/update GraphQL logic;
  it directly upserts raw device config rows/documents.
- Import ignores submitted `createdAt` and `updatedAt` values and writes current
  timestamps.
- Legacy aliases `exportDeviceConfigs` and `importDeviceConfigs` are not
  implemented.
- The concrete type strings are the `DeviceConfig.kt` constants such as
  `OPCUA-Client`, not the earlier example values such as `OPCUA`.

## Remaining Gaps And Risks

- The stale `dashboard/src/js/device-export-import.js` file still references
  unimplemented legacy operations. It is not used by
  `device-config-export-import.html`, but it can confuse future maintenance.
- The store-level import path bypasses per-device validation. Invalid
  type-specific `config` payloads can be persisted if they are structurally a
  JSON object.
- SQLite import currently fires asynchronous `executeUpdate` calls in a loop and
  resolves after a fixed 100 ms timer. This can under-report or race on slow
  storage.
- The GraphQL disabled-feature return for `importDevices` currently omits
  `imported`, `failed`, `total`, and `errors`, even though the schema marks them
  non-null.
- CrateDB export may return a raw string for `config` when the stored value does
  not start with `{`, but import requires `config` to be a JSON object.

## Optional Follow-Up Work

- Remove or update `dashboard/src/js/device-export-import.js`.
- Change the disabled-feature `importDevices` result to return all non-null
  fields.
- Replace SQLite's timer-based import completion with composed futures.
- Add type-specific validation before persisting imported configs, or explicitly
  document that import is a raw backup/restore path.
- Add compatibility aliases only if old dashboard builds need to keep working:

```graphql
type Query {
  exportDeviceConfigs(names: [String!]): [Device!]!
}

type Mutation {
  importDeviceConfigs(configs: [DeviceInput!]!): ImportDeviceConfigResult!
}
```
