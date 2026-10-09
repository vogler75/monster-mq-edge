# Plan: MonsterMQ HMI & Dashboard Hosting

This document defines the architecture, design rationale, GraphQL API contracts, storage integration, and implementation for hosting custom HTML/JS Human-Machine Interfaces (HMIs) and multi-dashboard applications directly within MonsterMQ and MonsterMQ Edge.

---

## 1. Overview & Core Objectives

MonsterMQ and MonsterMQ Edge allow edge devices (e.g. Siemens WinCC Unified Comfort Panels, Raspberry Pi 4/5) and central brokers to host HTML5-based industrial HMIs and SCADA dashboards directly without requiring an external web server like Nginx or Apache.

### Core Objectives
1. **Zero-Dependency Static Hosting**: Embed an HTTP static file server into the broker's GraphQL listener (port 4000/8080) under `/hmi/`.
2. **HMI as a First-Class Device Configuration**: Model each HMI application as a device configuration of type `"HMI"` in `DeviceConfigStore` (`deviceconfigs` table/collection), providing full enablement (`enabled == true/false`), lifecycle control (`start`/`stop`), and node assignment (`reassign`).
3. **Feature Flag (`Hmi`)**: Expose an `Hmi` feature flag across both Go (`monster-mq-edge`) and Kotlin (`monster-mq`) brokers to enable or disable HMI hosting functionality.
4. **GraphQL Schema Parity**: Standardize GraphQL query (`hmis`, `hmi`, `hmiFiles`, `exportHmiZip`) and mutation (`hmi: HmiMutations!`) interfaces across both brokers.
5. **Dashboard App Lifecycle & Zip Packaging**: Provide zip import/export (`uploadZip`, `exportHmiZip`) to deploy and back up complete dashboard applications.
6. **AI HMI Builder Skill**: Provide an AI skill (`monstermq-hmi-builder`) to assist AI assistants in generating responsive industrial HMI screens.

---

## 2. Architecture & File Layout

### 2.1 File System Structure
All HMI assets are stored under `./data/hmi/`:

```
./data/hmi/
├── main/                         # Main default HMI served at /hmi/
│   ├── index.html
│   └── app.js
├── dashboard1/                   # Secondary HMI served at /hmi/dashboard1/
│   ├── index.html
│   └── chart.js
└── app2/                         # Secondary HMI served at /hmi/app2/
    └── index.html
```

### 2.2 HTTP Routing & Gating (`/hmi/`)
The static HTTP handler mounts on the main GraphQL port (e.g., `:4000`):
- `http://<broker>:4000/hmi/` (or `/hmi`) -> Serves the main HMI (`isMain: true` or name `"main"`).
- `http://<broker>:4000/hmi/<name>/` -> Serves the named HMI app directory `./data/hmi/<name>/`.
- **Enablement Check**: The router inspects `Hmi` feature flag and `IsHmiEnabled(name)`. If the HMI device is disabled (`enabled == false`), the request is rejected with `404 Not Found`.

---

## 3. Device Configuration Model (Type: `"HMI"`)

HMI dashboard applications are stored as persistent device configurations in `DeviceConfigStore`:
- **`name`**: Dashboard identifier (e.g., `"main"`, `"dashboard1"`).
- **`type`**: `"HMI"`.
- **`nodeId`**: Target cluster node handling the HMI app (defaults to `"local"`).
- **`enabled`**: `Boolean` — controls whether the HTTP route is active.
- **`config`**: JSON object containing:
  - `urlPath`: Relative URL path (e.g. `""` for main, or `"dashboard1"`).
  - `isMain`: `Boolean` — whether this is the default dashboard served at `/hmi/`.
  - `title`: Display title.
  - `description`: Text description.
  - `entryPoint`: Entry file (default: `"index.html"`).

---

## 4. GraphQL Schema Definition (`schema-types.graphqls` / `schema.graphqls`)

### SDL Schema
```graphql
type HmiConfig {
    urlPath: String!
    isMain: Boolean!
    title: String
    description: String
    entryPoint: String
}

type Hmi {
    name: String!
    nodeId: String!
    enabled: Boolean!
    config: HmiConfig!
    createdAt: String!
    updatedAt: String!
    isOnCurrentNode: Boolean!
    fileCount: Int
    sizeBytes: Long
}

type HmiResult {
    hmi: Hmi
    success: Boolean!
    message: String
}

type DashboardFile {
    path: String!
    sizeBytes: Long!
}

input HmiConfigInput {
    urlPath: String
    isMain: Boolean
    title: String
    description: String
    entryPoint: String
}

input HmiInput {
    name: String!
    nodeId: String
    enabled: Boolean
    config: HmiConfigInput!
}

type HmiMutations {
    create(input: HmiInput!): HmiResult!
    update(name: String!, input: HmiInput!): HmiResult!
    delete(name: String!): HmiResult!
    start(name: String!): HmiResult!
    stop(name: String!): HmiResult!
    toggle(name: String!, enabled: Boolean!): HmiResult!
    reassign(name: String!, nodeId: String!): HmiResult!
    uploadZip(name: String!, zipBase64: String!, setAsMain: Boolean): HmiResult!
}

type Query {
    hmis(name: String, nodeId: String): [Hmi!]!
    hmi(name: String!): Hmi
    hmiFiles(name: String!): [DashboardFile!]!
    exportHmiZip(name: String!): String!
}

type Mutation {
    hmi: HmiMutations!
}
```

---

## 5. Subsystem Implementations

### 5.1 Go Broker (`monster-mq-edge`)
- **Feature Flag**: `FeaturesConfig.Hmi` in `internal/config/config.go`, `yaml-json-schema.json`, `config.yaml`, and `resolver.go`.
- **HMI Manager**: `internal/hmi/manager.go` uses `DeviceConfigStore` (`Type = "HMI"`) to manage dashboard devices and zip compression/extraction.
- **HTTP Static Router**: `internal/graphql/server.go` serves `/hmi/` and verifies `IsHmiEnabled(name)`.
- **GraphQL Resolvers**: `internal/graphql/resolvers/resolver.go` implements `HmiMutationsResolver` and `Query` resolvers. Registered in `gqlgen.yml`.

### 5.2 Java/Kotlin Broker (`monster-mq`)
- **Feature Flag**: `const val Hmi = "Hmi"` in `Features.kt` and `yaml-json-schema.json`.
- **Device Constant**: `const val DEVICE_TYPE_HMI = "HMI"` in `DeviceConfig.kt`.
- **GraphQL Resolvers**: `HmiClientConfigQueries.kt` and `HmiClientConfigMutations.kt` backed by `IDeviceConfigStore`.
- **GraphQL Server**: Wired `hmiQueries` and `hmiMutations` into `GraphQLServer.kt`.
- **System Config Page**: Updated `dashboard/src/js/system-config.js` and `system-config.html` to display HMI Hosting feature status.

---

## 6. Implementation Status & Verification

- **`monster-mq-edge` (Go)**: Code generated via `make gen`. Binary compiled via `make build` (**0 errors**).
- **`monster-mq` (Java/Kotlin)**: Kotlin classes and GraphQL schema compiled via `mvn test-compile` (**BUILD SUCCESS**).
- **Consolidated Documentation**: Unified design and protocol recorded in this plan.
