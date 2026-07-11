# internal/httpserver

A small, reusable **graceful HTTP server** shared by every drun-dash HTTP
binary (`control-plane`, and later `llm-proxy`).

Every binary needs the same lifecycle — bind a listener, serve a handler, and
shut down cleanly when the process is signalled. That lifecycle lives here so
the binaries differ only in the handler they serve.

## API

```go
srv := httpserver.New(cfg, handler, logger)
err := srv.Run(ctx) // blocks until ctx is cancelled, then drains gracefully
```

- `New(cfg *config.Config, h http.Handler, logger *slog.Logger) *Server`
  builds a server that listens on `cfg.HTTPAddr` and serves `h`. Pass `nil` for
  `logger` to fall back to `slog.Default()`. It does **not** bind the socket.
- `(*Server).Run(ctx context.Context) error` starts listening and blocks. When
  `ctx` is cancelled it stops accepting connections and drains in-flight
  requests within a bounded grace period, then returns. It returns `nil` on a
  clean shutdown, the listen error if it could not serve, or the shutdown error
  if draining exceeded the grace period.

## Design notes

- **Transport-agnostic.** `New` takes an `http.Handler`, so callers own routing
  and middleware (see [`internal/httpx`](../httpx)). This keeps the server
  reusable across binaries.
- **Bounded timeouts.** A `ReadHeaderTimeout` guards against slow-header
  attacks, and shutdown is capped by a grace period so a stuck request cannot
  block process exit forever.
- **Signal wiring lives in the binary.** The server takes a `context.Context`;
  each `main` derives it from `signal.NotifyContext(SIGINT, SIGTERM)`.

## Usage

```go
cfg, err := config.Load()
// ...
logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

r := chi.NewRouter()
r.Use(httpx.RequestID, httpx.Recoverer(logger), httpx.Logger(logger))
// ... routes ...

ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

if err := httpserver.New(cfg, r, logger).Run(ctx); err != nil {
    log.Fatal(err)
}
```
