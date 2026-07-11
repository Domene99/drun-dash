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
| `POST` | `/v1/chat/completions` | Reverse-proxy to the configured upstream, injecting the master credential (streaming) |

`/v1/chat/completions` forwards to `<upstream>/v1/chat/completions` and streams
the response back **unbuffered** (see [`broker`](./broker.go)):

- the method, path, query, and request body are copied through;
- **safe** request headers pass through verbatim — hop-by-hop headers
  (`Connection`, `Transfer-Encoding`, `Proxy-Authorization`, anything named in
  `Connection`, …) are dropped;
- the **master credential** is injected as the `Authorization` header — see
  below — **overwriting** any client-supplied `Authorization`;
- the upstream status code and response headers — including `Content-Type`, so
  `text/event-stream` is preserved — are propagated as-is;
- the body is flushed as it arrives, so **SSE** streaming responses reach the
  client chunk by chunk instead of being buffered to completion.

### Credential injection

Credential custody is the platform's core guarantee: the **master upstream key
lives only in the proxy, never on the client**. On every forwarded request the
broker resolves the upstream provider credential from the
[`internal/vault`](../vault) and sets

```
Authorization: Bearer <resolved secret>
```

on the upstream request, **replacing** any `Authorization` the client sent — so
a client can neither read the master key nor choose the value the proxy
presents. The raw secret is never logged (only the non-sensitive `secret_ref`
is) and never echoed in an error body.

The credential is resolved **per request** (not cached at startup), so rotation
takes effect immediately and the key is never retained in process state.

Per-agent scoping, budget metering, entitlement checks, and multi-endpoint
routing remain **out of scope** here — they need the endpoint store and land in
later batches.

### Upstream configuration

The upstream base URL and the vault `secret_ref` are read from the environment,
not from [`internal/config`](../config) (which is frozen and has no upstream
field):

| Variable                  | Meaning                                             |
| ------------------------- | --------------------------------------------------- |
| `LLM_UPSTREAM_BASE_URL`   | Base URL of the upstream provider (scheme + host, optional path prefix), e.g. `https://api.openai.com` for OpenAI. |
| `LLM_UPSTREAM_SECRET_REF` | Vault ref for the master credential to inject. Defaults to `vault://dev/anthropic`. Under the dev [`EnvVault`](../vault) backend it maps to `DRUNDASH_SECRET_ANTHROPIC`. |

`LLM_UPSTREAM_BASE_URL` is validated **once** at router construction. If it is
unset or invalid (missing/unsupported scheme, no host), the server still starts
and `/healthz` stays up, but the broker route answers `502` with a flat
`{"code":"upstream_unconfigured", ...}` envelope rather than forwarding to a
surprise default.

If the master credential cannot be resolved from the vault (missing or invalid
`secret_ref`), the request is **not forwarded** and the broker answers `502`
`{"code":"upstream_credential_unavailable", ...}` — never leaking the secret or
forwarding without a credential. A dial or transport failure while forwarding
yields `502` `{"code":"bad_gateway", ...}`.

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
Set the upstream base URL and provide the master credential the `secret_ref`
maps to before starting it:

```sh
LLM_UPSTREAM_BASE_URL=https://api.openai.com \
DRUNDASH_SECRET_ANTHROPIC=sk-... \
llm-proxy
```
