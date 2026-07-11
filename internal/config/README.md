# internal/config

Environment-based runtime configuration for every drun-dash binary
(`control-plane`, `llm-proxy`, and the `drun-dash` CLI).

Configuration comes exclusively from environment variables. Each variable has a
default tuned for the local Docker dev stack (see
[`deploy/docker`](../../deploy/docker/README.md)), so a fresh checkout runs with
no environment set. [`Load`](./load.go) applies defaults, validates the result,
and **fails fast** with a clear error on any missing or malformed value.

This package only reads and validates configuration. It does **not** open
database or Redis connections (that is batch 3, `internal/db`) and holds no
secrets/vault logic.

## Usage

```go
import "github.com/Domene99/drun-dash/internal/config"

cfg, err := config.Load()
if err != nil {
    log.Fatalf("config: %v", err)
}
// cfg.DatabaseURL, cfg.HTTPAddr, ...
```

`Load` returns a `*Config` or an error. Match failure modes with `errors.Is`
against `config.ErrMissingField` and `config.ErrInvalidValue`.

## Environment variables

| Env var           | Default (local dev)                                                          | Used by                  |
| ----------------- | --------------------------------------------------------------------------- | ------------------------ |
| `DATABASE_URL`    | `postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable`      | control-plane            |
| `REDIS_URL`       | `redis://localhost:6379/0`                                                   | control-plane, llm-proxy |
| `HTTP_ADDR`       | `:8080`                                                                      | control-plane            |
| `PROXY_HTTP_ADDR` | `:8081`                                                                      | llm-proxy                |
| `LOG_LEVEL`       | `info`                                                                       | all                      |
| `ENV`             | `development`                                                                | all                      |

The `DATABASE_URL` and `REDIS_URL` defaults match
[`deploy/docker/.env.example`](../../deploy/docker/.env.example) and the
connection strings in [`deploy/docker/README.md`](../../deploy/docker/README.md).

### Validation rules

`Load` (and `Config.Validate`) enforce:

- Every variable is non-empty. Setting a variable to `""` is treated as an
  explicit empty value and fails — it does **not** fall back to the default.
- `DATABASE_URL` is an absolute URL with a host and scheme `postgres` or
  `postgresql`.
- `REDIS_URL` is an absolute URL with a host and scheme `redis` or `rediss`.
- `HTTP_ADDR` and `PROXY_HTTP_ADDR` are valid `host:port` listen addresses; an
  empty host (e.g. `:8080`) binds all interfaces.
- `LOG_LEVEL` is one of `debug`, `info`, `warn`, `error`.
- `ENV` is one of `development`, `staging`, `production`.

## Testing

```bash
go test ./internal/config/...
```
