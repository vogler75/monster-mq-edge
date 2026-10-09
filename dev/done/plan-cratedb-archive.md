# Plan: CrateDB archive store for archive groups

Status: implemented (2026-10-06), tested against CrateDB 6.4.5

## Goal

Archive groups can use `archiveType: CRATEDB` for the history, like the Java
broker. Today the edge broker knows the enum values (`stores.ArchiveCrateDB`,
`stores.DatabaseConnectionCrateDB`, GraphQL `MessageArchiveType.CRATEDB`) but
`archive.Manager.buildArchiveStore` has no case for it, so a group with
`CRATEDB` fails to start.

Scope: the **history archive** only. A CrateDB last-value store
(`lastValType: CRATEDB`) is not planned, as for QuestDB.

## No GraphQL change

Everything needed already exists in the SDL: `MessageArchiveType.CRATEDB`,
`DatabaseConnectionType.CRATEDB`, `BrokerConfig.crateDbUrl` /
`crateDbUser`. The resolver currently returns empty strings for the two
`BrokerConfig` fields; they get filled from the new config section. No SDL
file is touched.

## Reference (Java broker)

- `../main/broker/src/main/kotlin/stores/dbs/cratedb/MessageArchiveCrateDB.kt`
  (DDL, insert, history, aggregation, stats, purge)
- `../main/broker/src/main/kotlin/handlers/ArchiveGroup.kt:516, 826`
  (wiring, error text when the CrateDB URL is missing)
- `../main/broker/src/main/kotlin/Monster.kt:770` (config section)

## Storage-layout parity (DDL copied from Java)

Table name = archive name in lower case (`ArchiveName(group)`, as for the
other stores):

```sql
CREATE TABLE IF NOT EXISTS <name> (
    topic VARCHAR,
    time TIMESTAMPTZ,
    payload_b64 VARCHAR INDEX OFF STORAGE WITH (columnstore = false),
    payload_obj OBJECT(IGNORED),
    qos INT,
    retained BOOLEAN,
    client_id VARCHAR(65535),
    message_uuid VARCHAR(36),
    PRIMARY KEY (topic, time)
)
```

Payload columns follow the Java rule: `PayloadFormat` DEFAULT writes base64
into `payload_b64` only; JSON writes valid JSON into `payload_obj` and falls
back to `payload_b64` for non-JSON payloads.

## Design

### Driver

CrateDB speaks the PostgreSQL wire protocol (port 5432). Use `pgx/v5`
(`pgxpool`), already a dependency; no new module. New package
`internal/stores/cratedb/` with:

- `Open(ctx, url, user, pass) (*DB, error)`: `jdbc:` prefix stripped and
  credentials merged as in `archive.postgresDSN` (move it to a shared helper
  or call it), ping with `SELECT 1`.
- `MessageArchive` implementing `stores.MessageArchive`.

CrateDB quirks to handle:

| Quirk | Handling |
|---|---|
| No transactions (`BEGIN`/`COMMIT` are accepted and ignored) | Never rely on rollback; one statement per batch. |
| Extended protocol / statement cache | Use `QueryExecModeExec` (no server-side prepared statement cache) if the default mode trips on CrateDB's type descriptions; decided in the integration test. |
| `OBJECT` column from a parameter | Pass the JSON as text with an explicit cast (`$4::OBJECT`); verify on a live CrateDB. |
| Eventual visibility (refresh interval ~1 s) | Acceptable for history reads; no `REFRESH TABLE` on the write path. Tests call `REFRESH TABLE` before reading. |
| Duplicate `(topic, time)` | `ON CONFLICT (topic, time) DO NOTHING`, as in Java. |

### Methods

| Method | SQL |
|---|---|
| `EnsureTable` | DDL above |
| `AddHistory` | one multi-row `INSERT ... VALUES (...),(...) ON CONFLICT DO NOTHING` per batch (chunked to keep the parameter count below 32k) |
| `GetHistory` | `SELECT ... WHERE topic LIKE $1 [AND time >= $2] [AND time <= $3] ORDER BY time DESC LIMIT n`; `#`/`+` mapped to `%` like `stores/postgres`; payload decoded `payload_obj` first, then base64 `payload_b64` |
| `GetAggregatedHistory` | Java's bucket expression (`DATE_TRUNC` for 1/60/1440 min, computed bucket for 5/15), `TRY_CAST(payload_obj['a']['b'] AS DOUBLE)` for fields, `COALESCE(TRY_CAST(payload_obj AS DOUBLE), TRY_CAST(decode(payload_b64,'base64') AS DOUBLE))` for the raw value. Field names validated against the same pattern the Postgres store uses before they go into the SQL text. |
| `GetArchiveStats` | `MIN(time)` and `DATE_TRUNC('day', time)` counts |
| `PurgeOlderThan` | `DELETE FROM <name> WHERE time < $1`, row count from the command tag |
| `Close` | no-op (the pool is owned by the manager handles) |

### Wiring

1. `internal/config/config.go`: `CrateDB: {Url, User, Pass}` (`CrateDBConfig`,
   same keys as Java), plus `yaml-json-schema.json`, `config.yaml.example`.
2. `internal/archive/manager.go`
   - `buildArchiveStore`: case `stores.ArchiveCrateDB`, using the group's
     named connection or the default `CrateDB` section; error text as in Java
     when neither is configured.
   - named-connection switch and default-connection switch: case
     `DatabaseConnectionCrateDB` opening `cratedb.Open`, added to
     `handles.owned`.
3. `internal/archive/db_connections.go`: `RequiredDatabaseConnectionTypes`
   maps `ArchiveCrateDB` to `DatabaseConnectionCrateDB`; the default
   connection list includes CrateDB when `CrateDB.Url` is set; the error text
   "can only be used with SQLite, Postgres, QuestDB, or MongoDB" gets CrateDB.
4. `internal/graphql/resolvers/resolver.go`: `CrateDbURL` / `CrateDbUser`
   from the config.
5. Nothing to do in `internal/scripting/db.go` (already treats CrateDB as a
   Postgres-wire connection).
6. Docs: README archive section and the store table in
   `winccoa/docs/architecture.md` (Figure 3 already lists CrateDB).

## Tests

- Unit (no DB): DSN normalisation, insert/aggregation SQL builders, field
  name validation, payload encode/decode for DEFAULT and JSON.
- Integration, skipped unless `MONSTERMQ_TEST_CRATEDB_URL` is set
  (`docker run -p 5432:5432 crate:5.x -Cdiscovery.type=single-node`):
  create table, insert a batch with a duplicate, `REFRESH TABLE`, history with
  and without wildcard, aggregation (raw value and JSON field), stats, purge,
  a full archive group started through `archive.Manager`.
- Parity check: a table written by the Java broker is read by the edge
  broker's `GetHistory` and the other way round.

## Out of scope

- `lastValType: CRATEDB`: not planned (owner decision 2026-10-06), as for
  QuestDB. A group with `CRATEDB` history uses another last-value store.
- MCP archive query tool (`internal/mcp/tools.go:381` accepts only SQLite and
  Postgres today).
- CrateDB stays unsupported as a config, session or retained store, matching
  the Java broker (eventual consistency).

## Effort

About one day: the store package (~400 lines, mostly adapted from
`stores/questdb` and the Java class), wiring and config (~100 lines), tests.
The open risk is pgx against CrateDB's PG wire implementation (exec mode and
the `OBJECT` parameter cast); the integration test settles both first.

## Differences to the Java broker (found 2026-10-06)

Same table, same columns, same keys, same history/stats/purge SQL and the same
aggregation buckets and column names. Rows written by one broker are readable
by the other. Deliberate differences, all verified on CrateDB 6.4.5:

| Point | Java (`MessageArchiveCrateDB.kt`) | Edge |
|---|---|---|
| JSON payload that is not an object (`42`, `[1,2]`, `"x"`) with `payloadFormat: JSON` | Written to `payload_obj`; CrateDB stores it as `{}` without an error. The value is lost. | Only JSON objects go to `payload_obj`; everything else goes base64 to `payload_b64`. |
| Raw value in aggregations (no field) | `TRY_CAST(decode(payload_b64,'base64') AS DOUBLE)`: `decode` returns hex text (`\x3230`), so the result is always NULL. | `encode(decode(payload_b64,'base64'),'escape')` restores the text, numbers aggregate. |
| Aggregate function | Inserted into the SQL as given | Whitelist AVG/MIN/MAX/COUNT/SUM, else AVG (as the Postgres store) |
| Field path quoting | Not escaped | `'` doubled |
| Daily stats date | `toLocalDateTime()` in the JVM time zone | UTC |
| Wildcards in history | `#` to `%` | `#` and `+` to `%` (as the edge Postgres store) |

The first two were bugs in the Java broker, fixed there the same way (also in its CrateDB last-value store).
