# internal/proxy

HTTP routing layer for the `llm-proxy` binary — drun-dash's OpenAI-compatible
LLM call broker. Every model call routes through the proxy so the control plane
can hold master credentials, meter spend, and enforce entitlement at the point
of each call.

[`NewRouter`](./router.go) builds a [chi](https://github.com/go-chi/chi) router
wrapped in the shared [`internal/httpx`](../httpx) middleware chain — request
IDs, panic recovery, and structured access logging — mirroring
[`cmd/control-plane`](../../cmd/control-plane).

## Routes

| Method | Path                   | Status | Body                                            |
| ------ | ---------------------- | ------ | ----------------------------------------------- |
| `GET`  | `/healthz`             | `200`  | `{"status":"ok"}`                               |
| `POST` | `/v1/chat/completions` | `501`  | flat `Error` `{"code":"not_implemented", ...}`  |

`/v1/chat/completions` is a **placeholder**: credential brokering and
provider routing are not implemented yet, so it reports `501 Not Implemented`
via [`httpx.WriteError`](../httpx/json.go). The real brokering logic lands in a
later batch.

## Usage

```go
import (
	"log/slog"

	"github.com/Domene99/drun-dash/internal/proxy"
)

var logger *slog.Logger // from internal/log
handler := proxy.NewRouter(logger)
```

The binary in [`cmd/llm-proxy`](../../cmd/llm-proxy) serves this handler with
[`internal/httpserver`](../httpserver) on `PROXY_HTTP_ADDR` (default `:8081`).
