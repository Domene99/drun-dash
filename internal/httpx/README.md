# httpx

Shared HTTP building blocks for the drun-dash binaries (`control-plane`,
`llm-proxy`): request-scoped IDs, structured request logging, panic recovery,
and JSON request/response helpers. Centralizing them here keeps batch 3 handlers
thin and their behavior consistent across services.

Standard library only — `net/http` and `log/slog`. No external logging or router
framework is imported.

## Middleware

Every middleware is a `func(http.Handler) http.Handler` (aliased as
`httpx.Middleware`), so it plugs directly into chi's `Router.Use` or any other
`net/http`-compatible router.

| Middleware | Purpose |
| --- | --- |
| `RequestID` | Reuse a valid inbound `X-Request-ID` or generate a random 128-bit one, store it in the request context (`RequestIDFromContext`), and echo it on the response. Unsafe inbound values (control chars, over-long) are rejected to prevent header/log injection. |
| `Logger(logger)` | Log one structured `slog` line per request: method, path, status, bytes, duration, and request ID. Pass `nil` for `slog.Default()`. |
| `Recoverer(logger)` | Recover panics, log them with a stack trace, and write a generic `500` JSON error. The panic value never reaches the client. Pass `nil` for `slog.Default()`. |

`RecovererWithOptions(RecovererOptions{Debug: true})` additionally echoes the
panic value in the response `details` — for local development only; leave
`Debug` off in production so internal details do not leak.

### Chi chain example

```go
import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/Domene99/drun-dash/internal/httpx"
)

func router(logger *slog.Logger) http.Handler {
	r := chi.NewRouter()

	// Order matters: RequestID first so the logger and recoverer can see the
	// ID; Recoverer last so it wraps the handlers most tightly.
	r.Use(httpx.RequestID)
	r.Use(httpx.Logger(logger))
	r.Use(httpx.Recoverer(logger))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return r
}
```

`chi` is not imported by this package; it appears only in this example. The
middleware works with any router built on `net/http`.

## JSON helpers

| Function | Purpose |
| --- | --- |
| `WriteJSON(w, status, v)` | Marshal `v`, set `application/json`, write `status`. Marshals into a buffer first, so an encode failure yields a clean 500 envelope instead of a half-written body. |
| `WriteError(w, status, code, message)` | Write the flat error envelope. |
| `WriteErrorDetails(w, status, code, message, details)` | Same, plus a structured `details` object. |
| `DecodeJSON(w, r, dst)` | Size-limited decode (`DefaultMaxBodyBytes`, 1 MiB) that rejects unknown fields and trailing data. |
| `DecodeJSONLimit(w, r, dst, maxBytes)` | `DecodeJSON` with a custom size cap. |

Predefined codes (`CodeBadRequest`, `CodeUnauthorized`, `CodeForbidden`,
`CodeNotFound`, `CodeConflict`, `CodeInternal`, `CodeNotImplemented`) mirror the
codes used in the OpenAPI contract.

Map a decode error straight to a 400:

```go
var body CreateSessionRequest
if err := httpx.DecodeJSON(w, r, &body); err != nil {
	httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
	return
}
```

### Error envelope

The envelope is **flat**, matching the OpenAPI `Error` schema
(`openapi/v1.yaml` → `#/components/schemas/Error`), which is used directly as
response bodies throughout the contract:

```json
{
  "code": "not_found",
  "message": "session does not exist"
}
```

With optional details:

```json
{
  "code": "bad_request",
  "message": "validation failed",
  "details": { "field": "name" }
}
```

> Note: the envelope is intentionally **not** wrapped in an outer
> `{"error": {...}}` object. The flat shape is what the merged OpenAPI contract
> defines, so handlers using these helpers stay contract-conformant.
