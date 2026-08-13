# Plan: MonsterMQ Edge HMI & Dashboard Hosting

This document outlines the architecture, design rationale, API contracts, and implementation for hosting custom HTML/JS Human-Machine Interfaces (HMIs) and multi-dashboard applications directly within the MonsterMQ Edge broker.

---

## 1. Overview & Goals

Edge devices (e.g. Siemens WinCC Unified Comfort Panels, Raspberry Pi 4/5) running MonsterMQ Edge host HTML5-based industrial HMIs and SCADA dashboards without requiring an external web server like Nginx or Apache.

### Core Objectives
1. **Zero-Dependency Static Hosting**: Embed an HTTP static file server into the broker's HTTP listener (port 4000) under `/hmi/`.
2. **Multi-Dashboard Support**: Allow managing multiple independent dashboard applications (e.g. `/hmi/`, `/hmi/solar-plant/`, `/hmi/boiler-room/`).
3. **GraphQL Data Access**: HMIs use the broker's standard GraphQL API (`/graphql`) over HTTP (queries/mutations) and WebSockets (`topicUpdates` subscriptions) for live data, topic history, and controls.
4. **Dashboard App Lifecycle & Packaging**: Provide GraphQL queries and mutations (`uploadDashboard`, `exportDashboard`) to import and export entire dashboard applications as base64-encoded `.zip` archives.
5. **AI Skill Integration**: Provide an AI skill (`monstermq-hmi-builder`) to assist AI assistants in creating and updating HMI screens for MonsterMQ Edge.

---

## 2. Architecture & File Layout

### 2.1 File System Structure
All HMI assets are stored under `./data/hmi/`:

```
./data/hmi/
├── metadata.json                 # Records the designated "main" dashboard (default: "main")
├── main/                         # Main dashboard served at /hmi/
│   ├── index.html
│   └── app.js
├── solar-plant/                  # Secondary dashboard served at /hmi/solar-plant/
│   ├── index.html
│   └── chart.js
└── boiler-room/                  # Secondary dashboard served at /hmi/boiler-room/
    └── index.html
```

### 2.2 Routing Strategy (`internal/graphql/server.go`)
The broker uses `go-chi/chi` on the existing GraphQL server port (4000 / 14000) to route incoming HTTP requests:

- `http://<broker>:4000/hmi/` (or `/hmi`) -> Serves from `./data/hmi/<main_dashboard>/` (default: `main`).
- `http://<broker>:4000/hmi/<dashboardname>/` -> Serves from `./data/hmi/<dashboardname>/` if `<dashboardname>` exists.
- Static assets within subdirectories (e.g. `/hmi/solar-plant/js/chart.js`) are served directly.

---

## 3. HMI Manager Subsystem (`internal/hmi/manager.go`)

The `hmi.Manager` handles dashboard CRUD, metadata persistence, file isolation, and zip compression/extraction.

### Operations
- `ListDashboards()`: Scans `./data/hmi/` and returns stats (name, isMain, path, file count, total size, mod time).
- `GetDashboard(name)`: Retrieves stats for a specific dashboard.
- `CreateDashboard(name, setAsMain)`: Initializes a new directory with a default HTML template.
- `DeleteDashboard(name)`: Deletes a dashboard directory (protects default `main` from deletion).
- `SetMainDashboard(name)`: Updates `metadata.json` so `/hmi/` aliases to the selected dashboard.
- `UploadDashboardZip(name, zipBase64, setAsMain)`: Unpacks a base64-encoded `.zip` into `./data/hmi/<name>/`.
- `ExportDashboardZip(name)`: Zips `./data/hmi/<name>/` and returns a base64 string.
- `ReadDashboardFile(name, relPath)` / `WriteDashboardFile(name, relPath, content)`: Safe file access with path traversal protection (`strings.HasPrefix(targetPath, basePath)`).

---

## 4. GraphQL Schema Extension (`internal/graphql/schema/schema.graphqls`)

The GraphQL API is extended to expose dashboard management to external dashboards, developer tools, and automated build scripts:

```graphql
type DashboardApp {
    name: String!
    isMain: Boolean!
    path: String!
    fileCount: Int!
    sizeBytes: Long!
    updatedAt: String
}

type DashboardAppResult {
    success: Boolean!
    message: String
    dashboard: DashboardApp
}

type DashboardFile {
    path: String!
    sizeBytes: Long!
}

type Query {
    dashboards: [DashboardApp!]!
    dashboard(name: String!): DashboardApp
    dashboardFiles(name: String!): [DashboardFile!]!
    exportDashboard(name: String!): String!
}

type Mutation {
    createDashboard(name: String!, setAsMain: Boolean): DashboardAppResult!
    deleteDashboard(name: String!): DashboardAppResult!
    setMainDashboard(name: String!): DashboardAppResult!
    uploadDashboard(name: String!, zipBase64: String!, setAsMain: Boolean): DashboardAppResult!
}
```

---

## 5. AI Skill (`monstermq-hmi-builder`)

An AI skill is provided in `.agents/skills/monstermq-hmi-builder/SKILL.md` for coding assistants. It contains:
- Industrial UI design tokens (dark theme `#0f172a`, card `#1e293b`, accents `#38bdf8`).
- JS code snippets for GraphQL HTTP queries and WebSocket subscriptions (`graphql-ws`).
- Complete single-file HMI boilerplate with live Chart.js integration.
