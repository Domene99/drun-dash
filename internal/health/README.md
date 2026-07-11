# internal/health

The readiness abstraction behind the control-plane's `GET /readyz` endpoint.

Liveness and readiness are different questions:

- **Liveness** (`GET /healthz`) — "is the process running?" A static 200 owned
  by each binary. Never gate it on dependencies, or a blip in the database will
  make an orchestrator kill an otherwise-healthy process.
- **Readiness** (`GET /readyz`) — "can the process serve traffic — are its
  dependencies reachable?" Dependency-backed, and what this package implements,
  so a load balancer can drain a pod whose database is down without restarting
  it.

## Checkers

A `Checker` verifies one dependency:

```go
type Checker interface {
    Check(ctx context.Context) error
}
```

`PingChecker` is the concrete checker for PostgreSQL; it pings the shared
`*db.Pool`. Because `db.Connect` is lazy (it validates the connection string
but does not dial), `Ping` is what actually confirms connectivity.

`CheckerFunc` adapts a plain function to `Checker`, which is handy in tests.

## Handler

`Handler` turns a set of named checkers into an `http.HandlerFunc`:

```go
r.Get("/readyz", health.Handler(
    health.Named{Name: "database", Checker: health.PingChecker{Pool: pool}},
))
```

Behavior:

- Every checker runs in order under its own **2s** deadline derived from the
  request context, so a hung dependency cannot block the endpoint.
- All pass → `200 {"status":"ready"}`.
- First failure → `503` with a flat `httpx.ErrorBody`
  (`{"code":"not_ready","message":"dependency \"database\" not ready: ..."}`);
  remaining checkers are skipped.
- No checkers → ready.

The 503 body reuses `httpx.WriteError`, so it matches the flat error envelope
served everywhere else in the API. The `not_ready` code is exported as
`health.CodeNotReady`.

## Tests

`health_test.go` covers the ready, not-ready, short-circuit, and per-check
timeout paths with fake checkers, and needs **no database** — it runs in the
default `go test ./...`.

[`db.Pool`]: ../db/db.go
[`httpx.ErrorBody`]: ../httpx/json.go
