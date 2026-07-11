# internal/redis

The single shared Redis access layer for drun-dash. Every binary that talks to
Redis obtains its client through this package, so connectivity lives in exactly
one place. Redis 7 backs the request-path machinery of drun-dash — budget
counters, short-lived credential/entitlement caches, and telemetry buffering —
and later batches build those on top of the [`Client`] exposed here.

This package owns **connectivity only**. It defines no counters, caches, rate
limiters, or telemetry buffers — those belong to later batches. Its shape mirrors
[`internal/db`](../db) so both connectivity layers feel identical.

## Client

`Connect` builds a [go-redis] client from `config.Config.RedisURL`. It parses and
validates the connection string but dials lazily, so it succeeds even when Redis
is momentarily unreachable; use `Ping` to verify a live connection and `Close` to
release the pool at shutdown.

```go
cfg, err := config.Load()
if err != nil {
    return err
}

client, err := redis.Connect(ctx, cfg)
if err != nil {
    return err
}
defer client.Close()

if err := client.Ping(ctx); err != nil {
    return err // Redis is not reachable.
}

// Client embeds *redis.Client, so the full command surface is available:
if err := client.Set(ctx, "k", "v", 0).Err(); err != nil {
    return err
}
n, err := client.Incr(ctx, "counter").Result()
```

`Client` embeds `*redis.Client`; the go-redis command methods (`Get`, `Set`,
`Incr`, `Eval`, …) are promoted for downstream packages, alongside the `Ping` and
`Close` lifecycle helpers.

`RedisURL` comes from the `REDIS_URL` environment variable (default
`redis://localhost:6379/0`, logical database 0); `rediss://` is also accepted for
TLS. See [`internal/config`](../config).

## Tests

Unit tests (`redis_test.go`) cover URL handling and error paths and need **no
Redis** — they run in the default `go test ./...`.

Integration tests (`integration_test.go`) exercise a real Redis 7 and are gated
behind the `integration` build tag. They **skip** (not fail) when the server is
unreachable, so a machine with no Redis still produces a green build.

```sh
# Start the local Postgres 16 + Redis 7 stack.
docker compose -f deploy/docker/docker-compose.yml up -d

# Run the integration tests against it.
go test -tags integration ./internal/redis/...
```

Override the target server with `REDIS_URL` (default: the local dev stack in
`config.DefaultRedisURL`).

[`Client`]: ./redis.go
[go-redis]: https://pkg.go.dev/github.com/redis/go-redis/v9
