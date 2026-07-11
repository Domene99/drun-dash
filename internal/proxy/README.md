# internal/proxy

HTTP layer for the `llm-proxy` binary — drun-dash's OpenAI-compatible LLM call
broker. Every model call routes through the proxy so the control plane holds
master credentials, meters spend, and enforces the per-session credential grant
at the point of each call.

[`NewRouter`](./router.go) builds a [chi](https://github.com/go-chi/chi) router
wrapped in the shared [`internal/httpx`](../httpx) middleware chain — request
IDs, panic recovery, and structured access logging — mirroring
[`cmd/control-plane`](../../cmd/control-plane). It takes the shared database
handle (`store.Querier`, so a `*db.Pool` satisfies it) that the broker's
per-call grant validation and metering resolve through.

## Routes

| Method | Path                   | Behavior                                                        |
| ------ | ---------------------- | -------------------------------------------------------------- |
| `GET`  | `/healthz`             | `200` `{"status":"ok"}`                                        |
| `POST` | `/v1/chat/completions` | Validate the credential grant, enforce the org's hard budget caps, reverse-proxy to the upstream (injecting the master credential), stream, and meter |

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

## Grant validation (ADR-0003 §3)

Every call is gated by a **per-session credential grant** minted at session
launch (`credential_grants`). The client presents its scoped proxy token as a
bearer token; the broker, on each request:

1. reads the presented bearer token — missing/malformed → **`401`**;
2. hashes it (`sha256Hex`, lowercase-hex SHA-256 of the full token — the exact
   form the launch handler stores) and looks the grant up by hash — unknown →
   **`401`**;
3. loads the owning session and rejects **fail-closed** if the grant is revoked,
   expired, or the session is `terminated`/`failed` → **`401`**;
4. enforces the grant's `scopes`: the route requires the `chat.completions`
   operation; a grant that does not authorize it → **`403`**.

Only after all four pass — and the org's budget check below — is the call
forwarded. The `401`/`403` bodies are flat `httpx` error envelopes
(`unauthorized` / `forbidden`), matching the OpenAPI `Error` contract.

## Budget enforcement (ADR-0003 §5)

After grant validation and **before** any credential injection or forwarding,
the broker enforces the calling org's **hard budget caps**. It resolves the
session's `org_id` (from the validated grant → session) and calls
`store.AnalyticsStore.BudgetUtilization`, which returns each budget rule's
current-window spend against its hard cap. If **any** rule has reached its cap
(`Spent >= Limit`), the call is **refused** with

```
429 {"code":"budget_exceeded", "message":"organization budget \"<name>\" ..."}
```

and the upstream is **never contacted** — so a blocked call injects no
credential and is never metered. This is the design's request-path spend
control: spend is stopped at the **next** call after a cap is hit. Rules with no
hard cap (a SQL-NULL limit) impose no ceiling and are skipped. The check **fails
closed**: if the utilisation lookup errors, the broker answers `500` rather than
forwarding, since it cannot prove the org is under cap.

**v1 scope.** Caps are enforced at the **org level** using the existing org-wide
aggregation. Two deliberate follow-ups:

- **Scoped caps.** Per-member/agent/team budget rules are not yet narrowed by
  scope here — every rule is compared against org-wide spend. The store already
  carries the rule scope; wiring it through the aggregation is the next step.
- **Redis running counter.** The check adds a DB aggregation to the hot path on
  every call. That per-call latency is accepted for v1 per the design's stated
  metering tradeoff; a Redis running counter (incremented at metering time) is
  the planned optimization to avoid re-aggregating on each request.

## Credential injection

Credential custody is the platform's core guarantee: the **master upstream key
lives only in the proxy, never on the client**. The credential to inject is
selected from the **grant's endpoint** (not a fixed process-wide ref): the
broker loads `model_endpoints` for `grant.endpoint_id`, resolves its
`secret_ref` through the [`internal/vault`](../vault), and sets

```
Authorization: Bearer <resolved secret>
```

on the upstream request, **replacing** any `Authorization` the client sent — so
a client can neither read the master key nor choose the value the proxy
presents. The raw secret is never logged (only the non-sensitive `secret_ref`
is) and never echoed in an error body.

The credential is resolved **per request** (not cached at startup), so rotation
takes effect immediately and the key is never retained in process state. If it
cannot be resolved (the grant names no endpoint, the endpoint cannot be loaded,
or the vault has no secret for the ref), the request is **not forwarded** and
the broker answers `502` `{"code":"upstream_credential_unavailable", ...}`.

## Metering (ADR-0003 §5)

After a successful (`2xx`) forwarded call the broker appends an append-only
`usage_events` row attributed through the grant: `org_id` and `agent_id` from
the session, `endpoint_id` from the grant, and `prompt_tokens`/
`completion_tokens`/`model` read from the upstream response body — both the
non-streamed JSON `usage` object and the trailing `usage` event of an SSE stream
(when the client opted into it) are recognized. Metering is best-effort: a
response that reports no usage records nothing, and a write failure is logged,
not surfaced to the client. Budget enforcement (above) reads these same
aggregated `usage_events` to decide whether the next call is under cap.

## Upstream configuration

The upstream base URL is read from the environment, not from
[`internal/config`](../config) (which is frozen and has no upstream field):

| Variable                | Meaning                                             |
| ----------------------- | --------------------------------------------------- |
| `LLM_UPSTREAM_BASE_URL` | Base URL of the upstream provider (scheme + host, optional path prefix), e.g. `https://api.openai.com` for OpenAI. |

`LLM_UPSTREAM_BASE_URL` is validated **once** at router construction. If it is
unset or invalid (missing/unsupported scheme, no host), the server still starts
and `/healthz` stays up, but the broker route answers `502` with a flat
`{"code":"upstream_unconfigured", ...}` envelope rather than forwarding to a
surprise default. A dial or transport failure while forwarding yields `502`
`{"code":"bad_gateway", ...}`.

The upstream *destination* remains a single configured host; the upstream
*credential* is now per-grant (above). Multi-endpoint routing (selecting the
destination per endpoint) lands in a later batch.

## Usage

```go
import (
	"log/slog"

	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/proxy"
)

var (
	logger *slog.Logger // from internal/log
	pool   *db.Pool     // from internal/db
)
handler := proxy.NewRouter(logger, pool)
```

The binary in [`cmd/llm-proxy`](../../cmd/llm-proxy) serves this handler with
[`internal/httpserver`](../httpserver) on `PROXY_HTTP_ADDR` (default `:8081`).
It opens the shared database pool (`DATABASE_URL`) for grant validation, budget
enforcement, and metering. Set the upstream base URL and provide the master
credential the endpoint's `secret_ref` maps to before starting it — under the dev
[`EnvVault`](../vault) backend `vault://dev/anthropic` maps to
`DRUNDASH_SECRET_ANTHROPIC`:

```sh
LLM_UPSTREAM_BASE_URL=https://api.openai.com \
DRUNDASH_SECRET_ANTHROPIC=sk-... \
llm-proxy
```
