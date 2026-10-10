# Seer Stone

A Wails v3 desktop SQL client for CockroachDB, PostgreSQL, and Snowflake. It supports multiple saved connection profiles and a query workspace.

## Run

Requires Go, Wails v3 CLI, and the platform prerequisites for Wails/webview builds.

```sh
wails3 setup
wails3 dev
```

The dev configuration uses the embedded frontend assets served by Wails/Go and enables the built-in MCP server. Connect an MCP client to `http://127.0.0.1:9099/mcp` while the app is running.

## Database connections

Use **＋** in the sidebar to add a connection and choose CockroachDB, PostgreSQL, or Snowflake. CockroachDB profiles use port `26257` by default; PostgreSQL uses `5432`. Snowflake profiles use an account identifier, username/password, and optional database, warehouse, schema, and role. Snowflake browser SSO/key-pair authentication is not currently exposed.

User-created profiles are stored in the current user's config directory under `seer-stone/connections.json`. Passwords are not persisted there; they remain in frontend memory for the current app session. Optional developer-local PostgreSQL-compatible URI profiles can be placed in gitignored `connections.local.json`, for example `{ "connections": [{ "name": "Production", "dbURI": "postgresql://user:password@host:26257/defaultdb?sslmode=verify-full" }] }`.

Select a profile, enter its password, and choose **Connect** to test it. The database selector lists databases visible to the account. Choose one to browse schemas, tables, views, and columns in the sidebar. The SQL workspace runs against the selected database. Query execution has a 60-second timeout; connection tests have a 12-second timeout. Connection pools are retained per profile/database while the app is running.

Query tabs and SQL text are saved locally per connection in the app's browser storage. Switching to a connection with no saved workspace opens a fresh `query.sql`; returning to a connection restores its tabs and queries. Query results are not persisted.

## Pi SQL assistant

The in-app Pi assistant receives the selected SQL dialect and loaded schema context, but not connection credentials or editor SQL. Its query and write opt-ins persist per connection across app restarts. For URI profiles from `connections.local.json`, those choices are stored separately in the user's `seer-stone/pi-permissions.json` config file; no credentials are copied there. **Edit connection → Allow Pi to query this database** is off by default. When explicitly enabled, Pi receives one narrowly-scoped query tool backed by a loopback-only Seer Stone bridge; the database password remains in the app process and is never passed to Pi. Queries are limited to one statement, 30 seconds, 200 rows, and bounded result sizes. **Allow Pi to modify data** is a separate option, also off by default; when enabled it permits DML (INSERT, UPDATE, DELETE, UPSERT, MERGE), but not DDL, permissions, transaction control, or multiple statements. The existing data-change confirmation preference (on by default) prompts in the app before each Pi write. Read queries use read-only transactions on PostgreSQL/CockroachDB. Snowflake does not support read-only transactions in this driver, so use a least-privilege role matched to the access you grant. Query results are sent to the configured Pi model when the tool is used. Disabling access or editing/deleting a profile revokes its running bridge.

PostgreSQL and CockroachDB use secure `verify-full` TLS by default. Use `disable` only for local development databases.

## Verify and regenerate bindings

```sh
go test ./...
wails3 generate bindings -b -d frontend/bindings .
```
