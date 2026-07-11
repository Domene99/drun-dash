# internal/db

The single shared PostgreSQL access layer for drun-dash. Every binary that
talks to the database obtains its connection pool and runs schema migrations
through this package, so connectivity lives in exactly one place. Later batches
(repositories, HTTP handlers, the `readyz` check) build on top of the [`Pool`]
exposed here.

This package owns **connectivity and migrations only**. It defines no
per-table repositories, queries, or domain types — those belong to later
batches.

## Connection pool

`Connect` builds a [`pgxpool`] from `config.Config.DatabaseURL`. It parses and
validates the connection string but dials lazily, so it succeeds even when
Postgres is momentarily unreachable; use `Ping` to verify a live connection and
`Close` to release the pool at shutdown.

```go
cfg, err := config.Load()
if err != nil {
    return err
}

pool, err := db.Connect(ctx, cfg)
if err != nil {
    return err
}
defer pool.Close()

if err := pool.Ping(ctx); err != nil {
    return err // Postgres is not reachable.
}

// Pool embeds *pgxpool.Pool, so the full query surface is available:
rows, err := pool.Query(ctx, "SELECT 1")
```

`Pool` embeds `*pgxpool.Pool`; the pgx query methods (`Query`, `QueryRow`,
`Exec`, `Begin`, …) are promoted for downstream repository packages, alongside
the `Ping` and `Close` lifecycle helpers.

## Migrations

Migrations are the golang-migrate SQL files in the repository-root
[`migrations/`](../../migrations) directory (`000001_*` … `000006_*`). This
package runs them via a `file://` source and the pgx/v5 database driver; it does
**not** embed or modify the SQL files.

```go
// Apply all pending migrations. "No change" (already up to date) is success.
if err := db.Migrate(cfg.DatabaseURL, db.DefaultMigrationsPath); err != nil {
    return err
}

// Report the current schema version.
version, dirty, err := db.Version(cfg.DatabaseURL, db.DefaultMigrationsPath)

// Roll back the last migration (steps <= 0 rolls all the way down).
if err := db.Down(cfg.DatabaseURL, db.DefaultMigrationsPath, 1); err != nil {
    return err
}
```

`DefaultMigrationsPath` is `"migrations"`, resolved relative to the process
working directory (the repository root for the server binaries). Any
`postgres://` / `postgresql://` URL is accepted; the scheme is rewritten to
`pgx5://` internally for golang-migrate while preserving credentials, host, and
query parameters such as `sslmode`.

## Tests

Unit tests (`db_test.go`) cover URL handling and error paths and need **no
database** — they run in the default `go test ./...`.

Integration tests (`integration_test.go`) exercise a real Postgres 16 and are
gated behind the `integration` build tag. They **skip** (not fail) when the
database is unreachable, so a machine with no Postgres still produces a green
build.

```sh
# Start the local Postgres 16 + Redis 7 stack.
docker compose -f deploy/docker/docker-compose.yml up -d

# Run the integration tests against it.
go test -tags integration ./internal/db/...
```

Overrides:

- `DATABASE_URL` — connection string (default: the local dev stack in
  `config.DefaultDatabaseURL`).
- `MIGRATIONS_PATH` — path to the migrations directory (default:
  `../../migrations`, relative to this package).

[`Pool`]: ./db.go
[`pgxpool`]: https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool
