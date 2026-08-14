# MonsterMQ Edge Broker — GraphQL Schema Parity & Adaptation Guide

This document specifies the exact schema and type adaptations required in the **MonsterMQ Edge Broker** GraphQL implementation to achieve 100% data-layer contract parity with the **MonsterMQ Main Broker**. 

By completing these adaptations, client applications (e.g. `mmqcli`, web SDKs, HMI dashboards) can interact with both Edge and Main brokers without needing conditional branching or custom payload formatting.

---

## 1. High Priority: Data Input & Contract Alignment

### 1.1 `PublishInput` (`retain` vs. `retained`)

- **Current Edge**: Accepts `retain: Boolean` and nullable `payload: String`.
- **Main Broker**: Accepts `retained: Boolean = false` and non-null `payload: String!`.
- **Required Adaptation in Edge Broker**:
  - Add `retained: Boolean` to `PublishInput`.
  - Support both `retained` and `retain` in Edge resolver logic for backward compatibility.
  - Ensure `payload` is supported consistently across both APIs.

```graphql
# Target Edge PublishInput Schema:
input PublishInput {
  topic: String!
  payload: String!
  payloadBase64: String
  payloadJson: JSON
  qos: Int = 0
  retained: Boolean = false  # <-- ADDED for Main Broker parity
  retain: Boolean            # Kept for backward compatibility
  format: DataFormat = JSON
}
```

---

## 2. Medium Priority: Response Field Standardization

### 2.1 `PublishResult` Response Object

- **Current Edge / Main Broker**: `{ success: Boolean!, topic: String!, timestamp: Long!, error: String }`

```graphql
# Target Edge PublishResult Schema:
type PublishResult {
  success: Boolean!
  topic: String!
  timestamp: Long!
  error: String
}
```

### 2.2 `PurgeResult` Response Object

- **Current Edge**: `purgedCount: Long!`
- **Main Broker**: `deletedCount: Long!`
- **Required Adaptation in Edge Broker**:
  - Add `deletedCount: Long!` to `PurgeResult` (or rename `purgedCount` to `deletedCount`).

```graphql
# Target Edge PurgeResult Schema:
type PurgeResult {
  success: Boolean!
  message: String
  deletedCount: Long! # <-- ADDED to match Main Broker
  purgedCount: Long!  # Optional legacy alias
}
```

---

## 3. Low Priority: Schema & Enum Consistency

### 3.1 `DatabaseConnectionType` Enum

- **Current Edge**: `enum DatabaseConnectionType { POSTGRES, MONGODB }`
- **Main Broker**: `enum DatabaseConnectionType { POSTGRES, MONGODB, SQLITE }`
- **Required Adaptation in Edge Broker**:
  - Add `SQLITE` to `DatabaseConnectionType` in Edge (useful for local SQLite message store/lastVal persistence).

```graphql
enum DatabaseConnectionType {
  POSTGRES
  MONGODB
  SQLITE # <-- ADDED to match Main Broker
}
```

### 3.2 `UpdateAclRuleInput` Partial Updates

- **Current Edge**: `username: String!` and `topicPattern: String!` are mandatory (`!`).
- **Main Broker**: `username: String` and `topicPattern: String` are optional (`String`).
- **Required Adaptation in Edge Broker**:
  - Make `username` and `topicPattern` optional (`String`) in `UpdateAclRuleInput` so partial ACL rule updates function identically.

```graphql
input UpdateAclRuleInput {
  id: String!
  username: String        # Changed from String! to String
  topicPattern: String    # Changed from String! to String
  canSubscribe: Boolean
  canPublish: Boolean
  priority: Int
}
```

### 3.3 `Topic` Type (`isLeaf` Field Parity)

- **Current Edge**: `Topic` type includes `isLeaf: Boolean!`.
- **Main Broker**: Missing `isLeaf: Boolean!`.
- **Required Adaptation**:
  - Add `isLeaf: Boolean!` to **Main Broker** `Topic` type so tree browsers operate identically on both nodes.

---

## 4. Summary Matrix of Schema Adaptations

| Type / Operation | Current Edge Field | Target Main Field | Recommended Action |
| :--- | :--- | :--- | :--- |
| `PublishInput` | `retain: Boolean` | `retained: Boolean` | Add `retained: Boolean` to Edge |
| `PublishResult` | `message: String` | `timestamp: Long!`, `error: String` | Add `timestamp` and `error` to Edge |
| `PurgeResult` | `purgedCount: Long!` | `deletedCount: Long!` | Add `deletedCount` to Edge |
| `DatabaseConnectionType` | `POSTGRES`, `MONGODB` | `POSTGRES`, `MONGODB`, `SQLITE` | Add `SQLITE` to Edge |
| `UpdateAclRuleInput` | `username: String!` | `username: String` | Make `username` optional in Edge |
| `Topic` | `isLeaf: Boolean!` | *(Missing)* | Add `isLeaf: Boolean!` to Main |

---

## 5. Verification Checklist

After applying the adaptations to the Edge Broker GraphQL schema and resolvers:

1. Re-generate `edge-sdl.gql` from the Edge Broker server.
2. Run schema comparison script to verify 0 discrepancies across common data operations.
3. Test publishing via `mmqcli set-value` against Edge Broker nodes.
