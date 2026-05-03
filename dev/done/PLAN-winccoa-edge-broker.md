# Plan: WinCC OA Client for Edge Broker

## Purpose

Implement the MonsterMQ WinCC Open Architecture client pattern in the edge broker. The
current broker implementation connects to a WinCC OA GraphQL server, subscribes to
datapoint query updates over GraphQL WebSocket subscriptions, transforms datapoint
names into MQTT topics, and republishes each value change into the broker.

This document first describes what MonsterMQ already has, then maps that design to
the edge broker implementation.

## Current MonsterMQ Implementation

### Files and Responsibilities

Backend:

- `broker/src/main/kotlin/stores/devices/WinCCOaConfig.kt`
  Defines the typed WinCC OA client configuration objects.
- `broker/src/main/kotlin/devices/winccoa/WinCCOaConnector.kt`
  Per-client verticle. Owns one connection to one WinCC OA GraphQL server.
- `broker/src/main/kotlin/devices/winccoa/WinCCOaExtension.kt`
  Cluster-aware coordinator. Loads device configs, deploys connectors, handles
  config changes, and forwards connector-published messages into MQTT.
- `broker/src/main/kotlin/graphql/WinCCOaClientConfigQueries.kt`
  GraphQL query resolver for listing WinCC OA clients.
- `broker/src/main/kotlin/graphql/WinCCOaClientConfigMutations.kt`
  GraphQL mutation resolver for create/update/delete/start/stop/reassign and
  address management.
- `broker/src/main/resources/schema-queries.graphqls`
  WinCC OA query-side GraphQL object schema.
- `broker/src/main/resources/schema-mutations.graphqls`
  WinCC OA mutation-side GraphQL input/result schema.
- `broker/src/main/kotlin/graphql/MetricsResolver.kt`
  Exposes current and historical WinCC OA metrics through fields on
  `WinCCOaClient`.
- `broker/src/main/kotlin/handlers/MetricsHandler.kt`
  Periodically discovers local WinCC OA connectors and stores their metrics.
- `broker/src/main/kotlin/Monster.kt`
  Deploys `WinCCOaExtension` when the `WinCCOa` feature flag is enabled.
- `broker/src/main/kotlin/Features.kt`
  Declares `Features.WinCCOa`.

Dashboard:

- `dashboard/src/pages/winccoa-clients.html`
- `dashboard/src/js/winccoa-clients.js`
- `dashboard/src/pages/winccoa-client-detail.html`
- `dashboard/src/js/winccoa-client-detail.js`
- `dashboard/src/js/sidebar.js`

### Configuration Model

All WinCC OA clients are stored as generic `DeviceConfig` rows with:

- `type = "WinCCOA-Client"`
- `name`: unique bridge/client name
- `namespace`: root MQTT topic namespace for published values
- `nodeId`: cluster node that owns the connector
- `enabled`: whether the connector should run
- `config`: WinCC OA specific JSON

The WinCC OA-specific config is `WinCCOaConnectionConfig`:

- `graphqlEndpoint`: HTTP GraphQL endpoint, for example
  `http://winccoa:4000/graphql`.
- `websocketEndpoint`: optional WebSocket endpoint. If omitted, it is derived
  from `graphqlEndpoint` by replacing `http://` with `ws://` and `https://`
  with `wss://`.
- `username` and `password`: optional login credentials.
- `token`: optional direct bearer token. If set, it skips login.
- `reconnectDelay`: reconnect delay in milliseconds, default `5000`.
- `connectionTimeout`: HTTP connect timeout in milliseconds, default `10000`.
- `messageFormat`: `JSON_ISO`, `JSON_MS`, or `RAW_VALUE`.
- `transformConfig`: datapoint-name-to-topic transformation rules.
- `addresses`: list of `WinCCOaAddress` subscriptions.

`WinCCOaAddress` fields:

- `query`: WinCC OA `dpQueryConnectSingle` query text, for example
  `SELECT '_original.._value', '_original.._stime' FROM 'System1:Test_*'`.
- `topic`: MQTT topic prefix under the client namespace.
- `description`: free text.
- `answer`: whether `dpQueryConnectSingle` should return initial answer rows.
- `retained`: whether republished MQTT messages are retained.

`WinCCOaTransformConfig` fields:

- `removeSystemName`: removes `System1:` prefix.
- `convertDotToSlash`: converts `.` to `/`.
- `convertUnderscoreToSlash`: converts `_` to `/`.
- `regexPattern` and `regexReplacement`: optional final regex replacement.

The transformer also trims trailing dots before conversion and trims leading or
trailing slashes after conversion.

### Connector Lifecycle

`WinCCOaConnector` starts with one `DeviceConfig` in its verticle config.

Startup sequence:

1. Parse `DeviceConfig` and `WinCCOaConnectionConfig`.
2. Validate required fields and options.
3. Create Vert.x `WebClient` for HTTP login.
4. Create Vert.x `WebSocketClient` for GraphQL subscriptions.
5. Register an event bus metrics endpoint at
   `EventBusAddresses.WinCCOaBridge.connectorMetrics(deviceName)`.
6. Complete verticle startup.
7. In the background, run:
   `authenticate() -> connectWebSocket() -> setupSubscriptions()`.

Authentication behavior:

- If `token` is configured, use it directly as bearer token.
- Else, if `username` and `password` are configured, send this HTTP GraphQL
  mutation to `graphqlEndpoint`:

```graphql
mutation {
  login(username: "...", password: "...") {
    token
    expiresAt
  }
}
```

- Else, use anonymous access.

Connection/reconnect behavior:

- On WebSocket close or exception, mark disconnected and schedule reconnect.
- Reconnect repeats authentication, WebSocket connection, and subscription setup.
- On stop, cancel reconnect timer, close WebSocket, close clients, clear
  subscriptions, and prevent further reconnect attempts.

### WinCC OA GraphQL WebSocket Protocol

The WinCC OA client uses the modern GraphQL over WebSocket protocol:

- WebSocket subprotocol: `graphql-transport-ws`.
- Initial client message:

```json
{"type":"connection_init","payload":{"Authorization":"Bearer <token>"}}
```

If no token exists, the payload is empty.

- Subscription start message:

```json
{
  "id": "client-sub-1",
  "type": "subscribe",
  "payload": {
    "query": "subscription { dpQueryConnectSingle(query: \"...\", answer: false) { values type error } }"
  }
}
```

- Expected acknowledgement: `connection_ack`.
- Expected data message: `next`.
- The connector also accepts legacy `data` messages for compatibility with
  older `subscriptions-transport-ws` servers.
- It logs `error`, `complete`, and `connection_error`.
- It responds to `ping` with `pong`.
- It recognizes legacy `ka` keep-alive messages.

Important distinction: MonsterMQ's own dashboard subscriptions also use
`graphql-transport-ws`. `GraphQLServer.kt` registers Vert.x
`GraphQLWSHandler` on the same `/graphql` path as HTTP GraphQL and configures
the HTTP server with WebSocket subprotocol `graphql-transport-ws`. Browser code
uses `new WebSocket(url, 'graphql-transport-ws')`, sends
`connection_init`, waits for `connection_ack`, and then sends `subscribe`.

### Subscription Payload Handling

Each configured address creates one GraphQL subscription:

```graphql
subscription {
  dpQueryConnectSingle(query: "<address.query>", answer: <address.answer>) {
    values
    type
    error
  }
}
```

The connector expects `payload.data.dpQueryConnectSingle.values`.

Expected `values` shape:

- Row 0 is the header, for example:
  `["", ":_original.._value", ":_original.._stime"]`
- Rows 1..N are datapoint rows, for example:
  `["System1:Test_000001.1", 1, "2025-10-06T18:03:05.984Z"]`

Processing:

1. Strip a leading `:` from header column names.
2. For each data row, use column 0 as the datapoint name.
3. Build a JSON object from remaining columns.
4. Publish the row to MQTT.

### MQTT Publishing

Topic construction:

```text
<device.namespace>/<address.topic>/<transformed datapoint name>
```

Example:

- Namespace: `winccoa/plant1`
- Address topic: `test`
- Datapoint: `System1:Area.Pump_1.Value`
- With `removeSystemName=true`, `convertDotToSlash=true`,
  `convertUnderscoreToSlash=false`
- MQTT topic: `winccoa/plant1/test/Area/Pump_1/Value`

Message formats:

- `JSON_ISO`: publish the datapoint data object unchanged.
- `JSON_MS`: convert timestamp-like string fields whose key contains `time` or
  `stime` to epoch milliseconds.
- `RAW_VALUE`: publish only the first data column. If the first value is a
  JSON Buffer object (`{"type":"Buffer","data":[...]}`), publish binary bytes.

The connector creates a `BrokerMessage` with:

- `qosLevel = 0`
- `isRetain = address.retained`
- `clientId = "winccoa-connector-<deviceName>"`

It publishes the message internally to
`WinCCOaExtension.ADDRESS_WINCCOA_VALUE_PUBLISH`. The extension consumes that
event bus address and calls `Monster.getSessionHandler()?.publishMessage(...)`
so normal broker delivery and archiving paths are used.

### Extension and Cluster Behavior

`WinCCOaExtension` is deployed once per broker node when `Features.WinCCOa` is
enabled.

Responsibilities:

- Initialize `IDeviceConfigStore` using `Monster.getConfigStoreType(config)`.
- Load enabled devices assigned to the current node.
- Filter to `DeviceConfig.DEVICE_TYPE_WINCCOA_CLIENT`.
- Deploy one `WinCCOaConnector` per matching device.
- Track `deployedConnectors[deviceName] = deploymentId`.
- Track `activeDevices[deviceName] = DeviceConfig`.
- Maintain local shared map `winccoa.device.registry`, mapping namespace to
  device name.
- Expose local connector list on
  `EventBusAddresses.WinCCOaBridge.connectorsList(currentNodeId)`.
- Consume config-change events on `winccoa.device.config.changed`.
- Consume connector-published values on `winccoa.value.publish`.

Supported config-change operations:

- `add`, `update`, `addAddress`, `deleteAddress`: undeploy and redeploy if the
  device is enabled and assigned to this node; otherwise undeploy if present.
- `delete`: undeploy.
- `toggle`: deploy or undeploy based on the new enabled flag.
- `reassign`: deploy if moved to this node, undeploy if moved away.

Current assignment check is strict: `device.nodeId == currentNodeId`.

### Metrics

The connector metric endpoint returns:

```json
{
  "device": "<deviceName>",
  "messagesInRate": 12.3,
  "connected": true,
  "elapsedMs": 1000
}
```

`MetricsHandler` periodically:

1. Requests local WinCC OA connector list via
   `EventBusAddresses.WinCCOaBridge.connectorsList(nodeId)`.
2. Requests each connector metric via
   `EventBusAddresses.WinCCOaBridge.connectorMetrics(deviceName)`.
3. Aggregates `messagesInRate` into broker-level `winCCOaClientIn`.
4. Stores individual `WinCCOaClientMetrics(messagesIn, connected, timestamp)`.

GraphQL exposes:

- `WinCCOaClient.metrics: [WinCCOaClientMetrics!]!`
- `WinCCOaClient.metricsHistory(from, to, lastMinutes)`

Current metrics are read from the metrics store; history uses
`metricsStore.getWinCCOaClientMetricsList(...)`. If metrics store is missing,
the resolver returns zero/current fallback or empty history.

### GraphQL Schema

Query schema is in `schema-queries.graphqls`.

Enums:

```graphql
enum WinCCOaMessageFormat {
  JSON_ISO
  JSON_MS
  RAW_VALUE
}
```

Output types:

```graphql
type WinCCOaTransformConfig {
  removeSystemName: Boolean!
  convertDotToSlash: Boolean!
  convertUnderscoreToSlash: Boolean!
  regexPattern: String
  regexReplacement: String
}

type WinCCOaAddress {
  query: String!
  topic: String!
  description: String!
  answer: Boolean!
  retained: Boolean!
}

type WinCCOaConnectionConfig {
  graphqlEndpoint: String!
  websocketEndpoint: String
  username: String
  token: String
  reconnectDelay: Long!
  connectionTimeout: Long!
  messageFormat: WinCCOaMessageFormat!
  transformConfig: WinCCOaTransformConfig!
  addresses: [WinCCOaAddress!]!
}

type WinCCOaClientMetrics {
  messagesIn: Float!
  connected: Boolean!
  timestamp: String!
}

type WinCCOaClient {
  name: String!
  namespace: String!
  nodeId: String!
  config: WinCCOaConnectionConfig!
  enabled: Boolean!
  createdAt: String!
  updatedAt: String!
  isOnCurrentNode: Boolean!
  metrics: [WinCCOaClientMetrics!]!
  metricsHistory(from: String, to: String, lastMinutes: Int): [WinCCOaClientMetrics!]!
}
```

Query:

```graphql
extend type Query {
  winCCOaClients(name: String, node: String): [WinCCOaClient!]!
}
```

Mutation schema is in `schema-mutations.graphqls`.

Grouped mutation root:

```graphql
extend type Mutation {
  winCCOaDevice: WinCCOaDeviceMutations!
}
```

Operations:

```graphql
type WinCCOaDeviceMutations {
  create(input: WinCCOaClientInput!): WinCCOaClientResult!
  update(name: String!, input: WinCCOaClientInput!): WinCCOaClientResult!
  delete(name: String!): Boolean!
  start(name: String!): WinCCOaClientResult!
  stop(name: String!): WinCCOaClientResult!
  toggle(name: String!, enabled: Boolean!): WinCCOaClientResult!
  reassign(name: String!, nodeId: String!): WinCCOaClientResult!
  addAddress(deviceName: String!, input: WinCCOaAddressInput!): WinCCOaClientResult!
  updateAddress(deviceName: String!, query: String!, input: WinCCOaAddressInput!): WinCCOaClientResult!
  deleteAddress(deviceName: String!, query: String!): WinCCOaClientResult!
}
```

Inputs:

```graphql
input WinCCOaTransformConfigInput {
  removeSystemName: Boolean = true
  convertDotToSlash: Boolean = true
  convertUnderscoreToSlash: Boolean = false
  regexPattern: String
  regexReplacement: String
}

input WinCCOaConnectionConfigInput {
  graphqlEndpoint: String = "http://winccoa:4000/graphql"
  websocketEndpoint: String
  username: String
  password: String
  token: String
  reconnectDelay: Long = 5000
  connectionTimeout: Long = 10000
  messageFormat: WinCCOaMessageFormat = JSON_ISO
  transformConfig: WinCCOaTransformConfigInput
  addresses: [WinCCOaAddressInput!]
}

input WinCCOaAddressInput {
  query: String!
  topic: String!
  description: String
  answer: Boolean = false
  retained: Boolean = false
}

input WinCCOaClientInput {
  name: String!
  namespace: String!
  nodeId: String!
  enabled: Boolean = true
  config: WinCCOaConnectionConfigInput!
}
```

Result:

```graphql
type WinCCOaClientResult {
  success: Boolean!
  client: WinCCOaClient
  errors: [String!]!
}
```

Resolver details:

- Queries return empty lists when `Features.WinCCOa` is disabled.
- Mutations return failed results when the feature is disabled locally or on the
  target node.
- Create rejects duplicate names.
- Update preserves `createdAt`, preserves existing addresses, and preserves the
  existing password if the update payload omits it.
- Address mutations manage addresses separately from the main update operation.
- Address identity is the `query` string.
- Successful writes publish `WinCCOaExtension.ADDRESS_DEVICE_CONFIG_CHANGED`.

### MonsterMQ Dashboard Behavior

The list page queries `winCCOaClients` and displays:

- bridge name
- GraphQL endpoint
- namespace
- node assignment/current-node indicator
- enabled status
- connection status from `metrics[0].connected`
- configured query count
- message rate from `metrics[0].messagesIn`
- actions

The detail page supports:

- creating and editing client config
- selecting cluster node from `clusterNodes`
- editing endpoint, credentials/token, reconnect/timeout, message format
- editing transform config
- managing addresses through separate add/update/delete mutations after save
- preserving password behavior on update

## Edge Broker Implementation Plan

### 1. Confirm Edge Broker Runtime Shape

Before coding, identify the edge broker equivalents for:

- device/config persistence
- MQTT publish API
- metrics collection and history
- HTTP GraphQL endpoint
- GraphQL WebSocket subscription server/client libraries
- feature flags or module enablement
- deployment/lifecycle model

If the edge broker is single-node only, keep the same schema fields but treat
`nodeId` as `"local"` or hide reassignment in UI/API. If it supports clustering,
preserve the MonsterMQ node assignment model.

### 2. Port the Data Model

Create edge equivalents for:

- `WinCCOaAddress`
- `WinCCOaTransformConfig`
- `WinCCOaConnectionConfig`
- generic device row or dedicated WinCC OA client table/model

Keep the public GraphQL names and field names identical to MonsterMQ unless the
edge broker already has a standard naming convention. This makes dashboard and
API reuse easier.

Implementation requirements:

- Validate endpoint schemes.
- Validate username/password are both present or both absent.
- Validate `messageFormat`.
- Validate each address has non-blank `query` and `topic`.
- Validate regex config eagerly.
- Derive WebSocket endpoint from HTTP endpoint when omitted.

### 3. Implement the Connector

Create one runtime connector per configured WinCC OA client.

Required state:

- config object
- HTTP client
- WebSocket client
- auth token
- connected/reconnecting/stopping flags
- reconnect timer
- active subscriptions map: subscription ID to address
- topic transform cache: datapoint name to MQTT topic
- message counter for metrics

Startup:

1. Validate config.
2. Register metrics endpoint or internal metrics callback.
3. Authenticate.
4. Connect WebSocket with subprotocol `graphql-transport-ws`.
5. Send `connection_init`.
6. After `connection_ack`, send all `subscribe` messages.

Prefer waiting for `connection_ack` before sending subscriptions in the edge
implementation. MonsterMQ currently calls `setupSubscriptions()` directly after
the WebSocket connect future completes, while `connection_ack` is handled
asynchronously. Some servers tolerate this, but strict `graphql-transport-ws`
servers expect subscriptions after acknowledgement.

### 4. Implement GraphQL Client Protocol Exactly

Use this wire protocol for WinCC OA:

- WebSocket subprotocol: `graphql-transport-ws`
- Init:

```json
{"type":"connection_init","payload":{"Authorization":"Bearer <token>"}}
```

- Subscribe:

```json
{
  "id": "<clientName>-sub-<n>",
  "type": "subscribe",
  "payload": {
    "query": "subscription { dpQueryConnectSingle(query: \"...\", answer: false) { values type error } }"
  }
}
```

- Handle: `connection_ack`, `next`, `error`, `complete`, `ping`.
- Send `pong` for `ping`.
- Optionally tolerate old server messages: `data`, `ka`, `connection_error`.

Authentication:

- Direct token: use as bearer token.
- Username/password: call HTTP GraphQL `login` mutation, extract `token`.
- Anonymous: send empty init payload.

Open question for edge: standardize auth payload casing. MonsterMQ's WinCC OA
connector sends `Authorization`; some dashboard code sends `authorization`.
Use `Authorization` for WinCC OA unless testing against the target WinCC OA
GraphQL server proves it requires lowercase.

### 5. Implement Data Parsing and MQTT Publishing

Replicate MonsterMQ parsing:

- Require `payload.data.dpQueryConnectSingle`.
- If `error` is present, log and skip.
- If `values` has fewer than two rows, skip.
- Use row 0 as headers.
- Strip leading `:` from header names.
- Convert each data row into a JSON object.
- Publish each row to:

```text
<namespace>/<address.topic>/<transformed datapoint>
```

Payload formatting:

- `JSON_ISO`: JSON encode the row object.
- `JSON_MS`: JSON encode with timestamp strings converted to epoch ms for
  keys containing `time` or `stime`.
- `RAW_VALUE`: publish first column only; support binary Buffer-shaped values.

MQTT publish behavior:

- QoS 0 unless edge broker config demands otherwise.
- Retain flag from address.
- Internal client ID equivalent to `winccoa-connector-<name>`.
- Route through the edge broker's normal publish path, not a shortcut, so ACLs,
  retained handling, subscriptions, and archiving/forwarding behave consistently
  with other internal publishers.

### 6. Implement Coordinator/Manager

If edge broker has a device manager, add WinCC OA there. Otherwise create a
WinCC OA manager service.

Responsibilities:

- Load enabled WinCC OA clients at startup.
- Start one connector per enabled client.
- Stop connectors on delete/disable.
- Restart connectors on config or address changes.
- Expose current connector list and metrics to the metrics subsystem.
- If clustered, only run clients assigned to the current node.
- If single-node, ignore reassignment internally but keep `nodeId = "local"` for
  API compatibility.

### 7. Implement GraphQL API

Expose the same schema as MonsterMQ where possible:

- `winCCOaClients(name, node): [WinCCOaClient!]!`
- grouped `winCCOaDevice` mutations
- nested `metrics` and `metricsHistory` fields

Keep separate output types and input types. Do not reuse output object types as
mutation inputs.

Required resolver behavior:

- Feature disabled -> empty query result or failed mutation result.
- Create -> validate, reject duplicate name, persist, start if enabled.
- Update -> validate, preserve password if omitted, preserve addresses unless
  edge intentionally supports full address replacement, persist, restart.
- Delete -> stop and remove.
- Start/stop/toggle -> persist enabled state and start/stop connector.
- Reassign -> only if edge supports multiple nodes; otherwise return a clear
  unsupported result or no-op for `local`.
- Address add/update/delete -> manage address list and restart connector.

### 8. Implement GraphQL WebSocket Server for Edge Broker

If the edge broker dashboard/API also needs live topic updates, match the
MonsterMQ GraphQL subscription protocol:

- Serve HTTP GraphQL and WebSocket GraphQL on the same configured path, usually
  `/graphql`.
- Use the `graphql-transport-ws` subprotocol.
- Browser clients connect with:

```javascript
new WebSocket(wsUrl, 'graphql-transport-ws')
```

- Client sends:

```json
{"type":"connection_init","payload":{}}
```

- Server sends `connection_ack`.
- Client starts subscriptions with `type: "subscribe"`.
- Server streams data as `type: "next"` with `payload.data`.
- Client unsubscribes with `type: "complete"`.
- Support `ping`/`pong`.

MonsterMQ dashboard subscription examples:

- `topicUpdates(topicFilters: [String!]!, format: DataFormat = JSON)`
- `topicUpdatesBulk(topicFilters, format, timeoutMs, maxSize)`
- `systemLogs(...)`

For parity, the edge broker should implement at least topic update
subscriptions if its dashboard needs live value viewing.

### 9. Implement Metrics

Expose current connector metrics:

- `messagesIn`: rate of WinCC OA datapoint rows published into MQTT.
- `connected`: current WebSocket connection state.
- `timestamp`: resolver timestamp or collected timestamp.

Add metrics history if the edge broker has a metrics store. If not, return
empty history and keep current metrics live.

Recommended internal sample:

```json
{
  "device": "oa-plant1",
  "messagesInRate": 4.2,
  "connected": true,
  "elapsedMs": 1000
}
```

### 10. Implement Dashboard Parity

If the edge broker uses the same dashboard stack, port:

- list page
- detail page
- sidebar entry
- GraphQL queries/mutations
- node selection behavior, adjusted for single-node if needed

Minimum list view:

- name
- GraphQL endpoint
- namespace
- node assignment
- enabled status
- connection status
- number of queries
- current messages/sec
- actions

Minimum detail view:

- connection settings
- credentials/token
- reconnect/timeout
- message format
- transform config
- address list with query/topic/answer/retained

### 11. Testing Plan

Unit tests:

- endpoint derivation: HTTP -> WS and HTTPS -> WSS
- config validation
- transform rules, including regex and trim behavior
- payload formatting for `JSON_ISO`, `JSON_MS`, `RAW_VALUE`
- parsing WinCC OA `values` arrays
- reconnection state transitions

Integration tests:

- mock WinCC OA HTTP GraphQL login
- mock GraphQL WebSocket server with `graphql-transport-ws`
- `connection_init` contains bearer token when expected
- connector sends one subscription per address
- `next` messages become MQTT publishes on expected topics
- close/error triggers reconnect and resubscribe
- start/stop/toggle and address mutations restart connector
- metrics report connected state and message rate

Compatibility tests:

- server sends `data` instead of `next`
- server sends `ka`
- server sends `ping`
- `dpQueryConnectSingle.error` is logged and not published
- empty/header-only `values` does not publish

### 12. Edge Implementation Milestones

1. Add config/data classes and validation.
2. Add connector with auth and GraphQL WebSocket subscription support.
3. Add MQTT publishing path and message formatting.
4. Add manager/coordinator with lifecycle operations.
5. Add GraphQL schema and resolvers.
6. Add metrics current/history integration.
7. Add dashboard list/detail pages if needed.
8. Add tests and a mock WinCC OA GraphQL WebSocket fixture.
9. Run against a real WinCC OA GraphQL server and verify topic output.

## Known Improvements to Consider While Porting

- Wait for `connection_ack` before sending subscriptions.
- Escape GraphQL query strings with a proper JSON/GraphQL string encoder rather
  than only replacing double quotes.
- Do not expose passwords in query responses. MonsterMQ query resolver currently
  includes password in the config map in one path; the mutation map omits it.
- Add TLS options if edge deployments need custom trust stores or insecure lab
  certificates.
- Add subscription acknowledgement/error correlation per subscription ID.
- Add configurable QoS if edge deployments require QoS 1.
- Add bounded topic cache or cache invalidation on transform changes.
