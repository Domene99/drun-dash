# drun-dash Overview

A longer, end-to-end walkthrough of how the pieces described in the root
[README](../README.md) and [ARCHITECTURE.md](./ARCHITECTURE.md) actually fit
together at runtime. This is the "follow one agent session from launch to audit"
tour; it describes only what ships in v1.

## The shape of the system

drun-dash is a governance overlay for the open-source
[drun](https://github.com/dmosc/drun) engine. drun runs locally; drun-dash is
the hosted plane that decides **what an agent may do**, **which credentials it
may use**, **how much it may spend**, and **what record it leaves behind**. The
engine is never modified — integration is over the external `drun-mcp` endpoint
only.

Three server binaries and two CLIs cooperate around one PostgreSQL database and
one Redis instance:

- **control-plane** — the system of record and the REST API.
- **llm-proxy** — the credential-brokering, spend-metering data path.
- **admin-ui** — the read-mostly operator console.
- **drun-dash** CLI — the developer's entry point.
- **policyctl** — the operator's offline policy-signing tool.

## Data flow: one governed session

### 1. Authenticate

A developer runs `drun-dash login`, which performs the OAuth 2.0
device-authorization flow (RFC 8628) against the control plane and stores a
short-lived session token in a `0600` file. Every later CLI call presents it as
`Authorization: Bearer <token>`. The admin UI authenticates operators via OIDC
and then calls the same REST API server-side. See
[ADR-0001](./adr/0001-authentication-and-session-model.md).

### 2. Author and sign policy

Policy boundaries are versioned and immutable in the control plane. Operators
produce **signed policy bundles** offline with `policyctl`: `keygen` mints an
Ed25519 keypair, `sign` computes the bundle's `content_hash` and Ed25519
`signature` (and re-verifies before it will emit the file). The verification-only
`pkg/policy` library is what the control plane and clients use to load and check
bundles — signing and verifying are deliberately separated.

### 3. Launch a governed session

`drun-dash sessions launch --agent <id>` calls `POST /sessions/launch`. The
control plane checks the caller's **entitlement** to that agent, then hands back
two things (see [ADR-0003](./adr/0003-session-launch-and-credential-grant.md)):

1. a **signed policy bundle** the client verifies before use; and
2. a scoped, one-time **`proxy_token`** — the session's LLM-proxy credential,
   printed once to stdout and never written to disk or logs.

Server-side, launch records a **credential grant** (`credential_grants`) keyed
by the SHA-256 of the proxy token, scoped to the operations the session may
perform, and bounded by the session lifetime.

### 4. Run — every model call goes through the proxy

The client points its OpenAI-compatible traffic at the **llm-proxy** and
presents the `proxy_token` as its bearer token. On **each** call the proxy:

1. validates the grant fail-closed — missing/unknown token → `401`; revoked,
   expired, or a terminated/failed session → `401`; a scope that doesn't cover
   `chat.completions` → `403`;
2. enforces the org's **hard budget caps** via the analytics store — any rule at
   its cap → `429 budget_exceeded`, and the upstream is never contacted (so a
   blocked call injects no credential and is never metered);
3. injects the **master provider credential** (overwriting any client-supplied
   `Authorization`), reverse-proxies to the upstream, and streams the response
   back unbuffered so SSE reaches the client chunk by chunk;
4. **meters** the call.

The developer's actual agent work happens locally: the drun-dash CLI talks to
the unmodified drun engine through `drun-mcp` over MCP Streamable HTTP on
`127.0.0.1:7273/mcp`.

### 5. Capture — trajectories flow outbound

drun-dash models each agent run as a **checkpoint tree** (git-like branches).
The CLI emits `TrajectoryEvent`s and POSTs them to the control-plane ingest API.
A **residency boundary** governs what crosses: `structure` events (tree shape +
metadata) are accepted by default; `payload` events (raw file bytes) are
**rejected** unless a caller explicitly opts into an authorized payload flow
(`pkg/trajectory`). This keeps file contents off the control plane by default.

### 6. Escalate — the approvals queue

When a governed decision fails closed, an **approval** is raised. Human reviewers
work the queue from the CLI (`approvals list` / `approvals decide`) or the admin
UI (`POST /approvals/{id}/decision`). Decisions are recorded; re-deciding a
settled request is a `409`.

### 7. Observe — spend and forensic replay

Spend is aggregated by the analytics store and queried via
`GET /analytics/spend` (`drun-dash spend --group-by agent|policy|principal`) or
the admin UI's Spend page. The admin UI's **trajectory replay** viewer renders a
session's captured decision graph for forensic review, and the Sessions page
acts as a flight recorder over governed runs.

## The parallel matrix engine

`drun-dash matrix run` fans a task across multiple governed sessions (the
"branches"), runs each in parallel, scores every branch with a user-supplied
objective command, promotes the winner, and terminates every session so no grant
outlives the run. In this v1 cut each branch is an independent
`POST /sessions/launch` — which already enforces entitlement, policy, budget, and
metering per branch. The drun **checkpoint-fork** optimization (forking all
branches from one shared checkpoint) is a documented follow-up.

## Where things live

| Concern | Package(s) |
| ------- | ---------- |
| REST API handlers | `internal/httpapi` |
| Auth / OIDC / tokens | `internal/auth`, `internal/authmw`, `internal/oidc`, `internal/clitoken` |
| LLM broker (proxy) | `internal/proxy`, `internal/vault` |
| Policy compile / verify | `internal/policycompile`, `pkg/policy` |
| Trajectory schema | `pkg/trajectory` |
| Matrix engine | `internal/matrix` |
| Admin UI | `internal/ui` (templ views under `internal/ui/views`) |
| CLI command tree | `internal/cli` |
| drun-mcp client | `pkg/drunmcp` |
| Persistence | `internal/store`, `internal/db`, `migrations/` |

For the exact REST surface, see [`openapi/v1.yaml`](../openapi/v1.yaml); for
running it all locally, see the [RUNBOOK](./RUNBOOK.md).

## Known gaps

The honest v1 limitations are listed once, in the root
[README](../README.md#known-gaps--follow-ups): `publishAgentVersion` → `501`,
org-scoped budget caps only, matrix launches N sessions (no checkpoint-fork),
payload capture off by default, CLI policy-sync/engine-control stubs, and a
read-heavy admin UI.
