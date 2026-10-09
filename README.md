# Seer Stone

A Wails v3 desktop SQL client starter, currently focused on CockroachDB. It supports multiple saved cluster profiles and a query workspace.

## Run

Requires Go, Wails v3 CLI, and the platform prerequisites for Wails/webview builds.

```sh
wails3 setup
wails3 dev
```

The dev configuration uses the embedded frontend assets served by Wails/Go and enables the built-in MCP server. Connect an MCP client to `http://127.0.0.1:9099/mcp` while the app is running.

## CockroachDB connections

Use **＋** in the sidebar to add a cluster. Profiles include name, host, port (default `26257`), database, username, and SSL mode. User-created profiles are stored in the current user's config directory under `seer-stone/connections.json`. Passwords are intentionally not persisted there; they are retained in frontend memory only for the current app session. Optional developer-local URI profiles can be placed in `connections.local.json` (gitignored), using `{ "connections": [{ "name": "Production", "dbURI": "postgresql://user:password@host:26257/defaultdb?sslmode=verify-full" }] }`. These profiles appear automatically in the sidebar.

Select a profile, enter its password, and choose **Connect** to test it. After connecting, the database selector lists databases visible to that CockroachDB user (from `SHOW DATABASES`). Choose one to browse its user schemas, tables, views, and columns (type, nullability, and default) in the sidebar; use the filter and refresh controls as needed. The SQL workspace runs against the selected database. Enter a SQL statement and select **Run query** (or press Cmd/Ctrl+Enter). Query execution has a 60-second timeout. Connection tests have a 12-second timeout.

CockroachDB's secure `verify-full` TLS mode is the default. Use `disable` only for local development databases. This first MVP does not yet provide a system keychain, certificate-file configuration, SSH tunnels, query history, or persistent connection pools.

## Verify and regenerate bindings

```sh
go test ./...
wails3 generate bindings -b -d frontend/bindings .
```
