# internal/proxy

HTTP layer for the `llm-proxy` binary — drun-dash's OpenAI-compatible LLM call
broker. Every model call routes through the proxy so the control plane can hold
master credentials, meter spend, and enforce entitlement at the point of each
call.

[`NewRouter`](./router.go) builds a [chi](https://github.com/go-chi/chi) router
wrapped in the shared [`internal/httpx`](../httpx) middleware chain — request
IDs, panic recovery, and structured access logging — mirroring
[`cmd/control-plane`](../../cmd/control-plane).

## Routes

| Method | Path                   | Behavior                                                        |
| ------ | ---------------------- | -------------------------------------------------------------- |
| `GET`  | `/healthz`             | `200` `{"status":"ok"}`                                        |
| `POST` | `/v1/chat/completions` | Reverse-proxy passthrough to the configured upstream (streaming) |

`/v1/chat/completions` forwards to `<upstream>/v1/chat/completions` and streams
the response back **unbuffered** (see [`broker`](./broker.go)):

- the method, path, query, and request body are copied through;
- **safe** request headers pass through verbatim — hop-by-hop headers
  (`Connection`, `Transfer-Encoding`, `Proxy-Authorization`, anything named in
  `Connection`, …) are dropped;
- the upstream status code and response headers — including `Content-Type`, so
  `text/event-stream` is preserved — are propagated as-is;
- the body is flushed as it arrives, so **SSE** streaming responses reach the
  client chunk by chunk instead of being buffered to completion.

This is an **honest passthrough**: the client's own `Authorization` header is
forwarded unchanged. Master-credential injection, per-agent scoping, budget
metering, entitlement checks, and multi-endpoint routing are deliberately **out
of scope** here — they need the credential vault + endpoint store and land in
later batches.

### Upstream configuration

The single upstream base URL is read from the environment, not from
[`internal/config`](../config) (which is frozen and has no upstream field):

| Variable                | Meaning                                             |
| ----------------------- | --------------------------------------------------- |
| `LLM_UPSTREAM_BASE_URL` | Base URL of the upstream provider (scheme + host, optional path prefix), e.g. `https://api.openai.com` for OpenAI. |

It is validated **once** at router construction. If it is unset or invalid
(missing/unsupported scheme, no host), the server still starts and `/healthz`
stays up, but the broker route answers `502` with a flat
`{"code":"upstream_unconfigured", ...}` envelope rather than forwarding to a
surprise default. A dial or transport failure while forwarding yields `502`
`{"code":"bad_gateway", ...}`.

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
Set `LLM_UPSTREAM_BASE_URL` before starting it:

```sh
LLM_UPSTREAM_BASE_URL=https://api.openai.com llm-proxy
```
