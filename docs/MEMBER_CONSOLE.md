# Member Console — walkthrough

The **member console** (`cmd/member-console`, `internal/memberui`) is drun-dash's
second frontend. Where the [admin dashboard](./RUNBOOK.md) (`cmd/admin-ui`,
`:8082`) is the read-mostly surface an **operator** uses to *govern* the system,
the member console is the surface an ordinary org **member** uses to *run a
governed agent* — a conversational console where the member types a
natural-language directive and watches a policy-bound agent carry it out.

This document walks a newcomer from a cold checkout to a governed run: what the
console is, what it reads from the environment, how to bring it up against the
org stack and a local drun engine, and what the end-to-end governed run looks
like. It also states, plainly, what is implemented on `main` today versus what
[ADR-0004](./adr/0004-member-conversational-console-architecture.md) locks for
the batches still landing.

> **Companion script.** [`scripts/demo/member-console.sh`](../scripts/demo/member-console.sh)
> checks your prerequisites and prints the ordered bring-up commands. Run it
> first; this doc explains what each step means and guarantees.

---

## 1. What the console is (and is not)

The console is a **local client the member runs on their own workstation**, not
a hosted service. This is the whole point of the design's data-residency
posture (ADR-0004 §1, `docs/security/threat-model.md`): the drun engine and the
member's private code live on the member's machine and must never be forced
off it. So the console:

- **runs locally** and serves a small server-rendered web UI (Go + templ +
  HTMX) the member opens in their browser;
- **drives a local drun engine** for all code and execution — private bytes stay
  on-box;
- **reaches *out* to the org** for exactly the three things that must be
  centralized: **identity** (control-plane login), **model access** (the
  governed LLM proxy), and **governance** (the signed policy bundle, trajectory,
  and approvals).

What leaves the machine is deliberately narrow: a short-lived session token, the
model traffic that the proxy brokers, and **structure-only** trajectory/approval
metadata — the *shape* of what the agent did (tool names, sizes, exit codes),
not the raw file contents or commands. Raw payloads leave only when the verified
policy explicitly permits it (ADR-0004 §5).

It is a **separate binary** from the control plane and never imports
control-plane packages: it talks to the control plane only over HTTP, mirroring
the admin-ui pattern (`internal/ui`) rather than sharing it.

### Relationship to the admin dashboard

| | Admin dashboard (`admin-ui`) | Member console (`member-console`) |
| --- | --- | --- |
| Audience | Operators / admins | Org members |
| Deployment | Hosted container in the app stack (`:8082`) | Local binary on the member's own machine (`:8083`) |
| Auth | OIDC (operator signs in, or pastes a session token) | RFC 8628 device flow (approved in the admin UI) |
| Purpose | *Govern* — policy, spend, sessions, approvals, trajectory replay | *Run* — launch and drive a governed agent |
| Data path | Reads the control-plane REST API | Reads the control plane **and** drives a local drun engine |

The two are complementary: a member launches and runs an agent in the console;
the operator sees that run's spend, trajectory, and any escalations in the admin
dashboard.

---

## 2. Prerequisites

1. **The org stack up.** The control-plane (`:8080`) and llm-proxy (`:8081`)
   must be running and healthy — the console has nothing to log in to or launch
   against without them. Bring them up with the app stack in
   [`docs/RUNBOOK.md`](./RUNBOOK.md) (`deploy/docker/docker-compose.app.yml`:
   Postgres + Redis + migrate + control-plane + llm-proxy + admin-ui). The
   admin-ui (`:8082`) is needed too, because that is where the member **approves**
   their device-login request.
2. **OIDC configured on the control plane.** The device-authorization flow is
   approved on the admin UI's device-approval page, which sits behind OIDC. Set
   `CP_OIDC_ISSUER` and `CP_OIDC_CLIENT_ID` on the control plane (RUNBOOK §8);
   with them unset the control plane serves only health probes and the approval
   page will not mount, so login cannot complete.
3. **A reachable drun engine.** The governed run executes tools in a **local**
   drun sandbox over `drun-mcp` (MCP Streamable HTTP). The endpoint is
   `DRUN_MCP_ENDPOINT`, defaulting to
   [`drunmcp.DefaultEndpoint`](../pkg/drunmcp/client.go) —
   `http://127.0.0.1:7273/mcp` (port `7273` is fixed in drun today). drun-mcp has
   **no** `/health`; liveness is an MCP `Initialize` probe.
   > **On `main` today** the console shell (§4) does not yet dial the engine, so
   > you can bring the console up and log in without one. The engine becomes a
   > hard requirement when the conversational turn loop lands (ADR-0004 §4).
4. **A Go toolchain** to run the console from source (`go run ./cmd/member-console`)
   or `make build` to compile it into `./bin`. The console is **not** part of the
   docker app stack — it is a local binary by design, so you start it yourself.

---

## 3. Environment variables the console reads

Verified against [`cmd/member-console/main.go`](../cmd/member-console/main.go)
and [`internal/memberui`](../internal/memberui/README.md).

| Variable | Default | Purpose |
| --- | --- | --- |
| `MEMBER_CONSOLE_HTTP_ADDR` | `:8083` | Console listen address (`host:port`; validated at boot) |
| `CONTROL_PLANE_BASE_URL` | `http://localhost:8080` | Control-plane REST API base URL |
| `MEMBER_CONSOLE_CLIENT_ID` | `drun-dash-member-console` | Device-flow `client_id` (present-only, not confidential) |

The console also calls the shared `config.Load()`, so the same
`DATABASE_URL` / `REDIS_URL` / `LOG_LEVEL` / `ENV` variables the other binaries
read are validated at boot even though the console never dials Postgres or Redis
— the RUNBOOK dev defaults are valid, so it passes. `ENV=production` turns on
**Secure** cookies (HTTPS only); plain-HTTP localhost development leaves them
off so the session cookie is still sent back.

### Planned, not yet wired — do not expect these on `main`

ADR-0004 names two more knobs for the batches that add the conversational run.
They are documented here for accuracy, but **cross-checking the code shows they
are not read yet**:

- **`DRUN_MCP_ENDPOINT`** (default `http://127.0.0.1:7273/mcp`) — the local
  drun-engine address. It appears in ADR-0004 §1 as the console's engine-discovery
  knob but is **not** yet read by `cmd/member-console`/`internal/memberui`; only
  `pkg/drunmcp.DefaultEndpoint` exists today. (ADR-0004 also notes a naming split
  to reconcile: `pkg/drunmcp`'s integration tests use `DRUNMCP_ENDPOINT`.)
- **`MEMBER_CONSOLE_MODEL`** — a model-selection knob. It does **not** exist in
  the code or in ADR-0004; the model is whatever the governed proxy forwards
  upstream. Treat it as a not-yet-designed follow-up, not a current setting.

---

## 4. The flow, end to end

The member's journey is: **log in → pick an entitled agent → launch a governed
session → chat a directive → watch the governed run → review it in the admin
dashboard.** Steps 1–3 are implemented on `main` today; the conversational run
(steps 4–5) is the ADR-0004 design landing across the next batches — each step
below marks which.

### Step 1 — Log in (device flow, RFC 8628) — *implemented*

Open `http://localhost:8083`. Unauthenticated, the console redirects to
`/login`, which calls `POST /auth/device/code` and shows a **user code** and a
**verification link**. Approve it in the admin UI's device-approval page (behind
OIDC), which binds the grant to your identity. An HTMX poller hits `/login/poll`
(→ `POST /auth/device/token`); on approval the minted control-plane session JWT
is stored in a secure, HttpOnly cookie and you land on the home page.

The console shares the CLI's session model: it is the same control-plane session
token for the same machine (ADR-0004 §2). `drun-dash login` and the console log
in against the same control plane.

### Step 2 — Pick an entitled agent — *implemented*

The home page (`GET /`) lists the agents **you** are entitled to launch. The
client prefers `GET /me/agents` (the caller's entitled agents) and falls back to
`GET /agents` when that endpoint is absent (feature-detected on a 404). You can
only ever launch what you are entitled to; the console never invents an agent id.

### Step 3 — Launch a governed session — *implemented*

Choosing an agent posts to `/sessions` → `POST /sessions/launch` (ADR-0003). The
control plane checks your entitlement and returns:

- a **signed policy bundle** (the client verifies it fail-closed before use);
- a scoped, one-time **`proxy_token`** — the session's LLM-proxy credential;
- `proxy_base_url`, `fan_out_ceiling`, and `expires_at`.

The `proxy_token` is a **secret**: the console holds it **server-side, in
memory** and never renders it to the page or writes it to logs. Only the session
id, proxy base URL, fan-out ceiling, and expiry are shown. `POST
/sessions/{id}/terminate` ends the session and revokes the grant.

### Step 4 — Chat a directive & watch the governed run — *forthcoming (ADR-0004 §4)*

This is the conversational surface still landing. The member types a directive
("upgrade this service to Go 1.22 and run the tests"); the console drives a
**tool-calling loop**:

1. call the model **through the governed proxy** —
   `POST {proxy_base_url}/v1/chat/completions` with `Authorization: Bearer
   <proxy_token>` and a tool schema. **The model reasons centrally; tools execute
   locally.**
2. execute each returned tool call **in the local drun sandbox** via
   `pkg/drunmcp` (`session_bash`, `session_read_file`, `session_write_file`,
   `session_fork`, `session_diff`, `session_commit`, checkpoint/history/tree,
   and peers), intersected with the policy bundle's `allowed_tools`;
3. feed results back and loop until a final answer, a bound
   (`fan_out_ceiling` / `expires_at` / max iterations), or a policy block.

The console surfaces these as **live steps** and checkpoints before risky
actions so a bad step can be rolled back rather than corrupting the workspace.

### Step 5 — See the trajectory in the admin dashboard — *forthcoming (ADR-0004 §5)*

As the loop runs, the console emits **structure-only** `TrajectoryEvent`s to
`POST /sessions/{sessionId}/trajectory` (best-effort, batched, off the critical
path). Operators then see the run — its decision graph, spend, and any
escalations — in the admin dashboard's Sessions/trajectory-replay views. The
`session_id` is the correlation key tying the proxy calls, trajectory, and
approvals together.

---

## 5. Governance guarantees at each step

The console **routes through** the existing governance rails; it never routes
around them (ADR-0004 §4, §7).

- **Login (identity).** The device flow binds the session to the member's
  identity via the OIDC-gated approval page. The session token authenticates
  control-plane calls only — it is **never** the model credential (distinct
  audiences; ADR-0001).
- **Launch (policy).** The client **verifies the signed policy bundle
  fail-closed** before anything runs — no verified bundle, no run (ADR-0003 §2).
  The bundle's `allowed_tools` is what bounds the tool set offered to the model.
- **Model calls (brokering / metering / budget).** Every model call goes through
  the llm-proxy with the scoped `proxy_token`. The proxy validates the grant
  fail-closed, enforces the org's hard **budget caps** (`429 budget_exceeded`,
  upstream never contacted), **injects the master provider credential**
  server-side (the laptop never holds it), reverse-proxies, and **meters** the
  call (ADR-0003 §3, `docs/OVERVIEW.md` §4). Direct-to-provider calls are
  designed out.
- **Tools (sandbox + residency).** Tools run inside the local drun sandbox
  constrained by the compiled policy config; code and execution stay on-box.
- **Trajectory (structure-only residency boundary).** `field_class: structure`
  is the default and the residency-preserving one — shape and metadata leave the
  box, raw payloads do **not**, and are promoted to `payload` only when the
  verified policy permits (ADR-0004 §5).
- **Escalations (fail-closed → approval, not a dead error).** When the proxy
  rejects a call or a tool falls outside `allowed_tools`, the console raises
  `POST /approvals` with **structure-only** context and **pauses that step** — it
  does not execute the blocked action or fabricate a result. An admin decides in
  the dashboard (`POST /approvals/{id}/decision`); on approval the step proceeds,
  on denial a governed "action denied" result is fed back to the model (ADR-0004
  §6).

Pointers: **ADR-0001** (auth, device flow, distinct audiences, fail-closed),
**ADR-0003** (launch, signed bundle + fail-closed verify, scoped `proxy_token`,
metering), **ADR-0004** (the console architecture: local client, turn loop,
structure-only trajectory, escalations).

---

## 6. Current limitations / follow-ups

Stated honestly, matching what is on `main`:

- **The conversational run view is not built yet.** Today the console is the
  **shell**: device-flow login, the entitled-agent list, and session
  launch/terminate. The chat surface, the tool-calling turn loop, live steps,
  trajectory emission, and in-UI escalations are the ADR-0004 design landing in
  later batches (see `internal/memberui/README.md`: "the conversational run view
  arrives in a later batch").
- **`DRUN_MCP_ENDPOINT` / `MEMBER_CONSOLE_MODEL` are not wired** (see §3). The
  engine-discovery knob is designed but unread; the model knob does not exist.
- **The checkpoint timeline, diff-before-commit, full escalation
  approve-resume, and the parallel matrix view are landing in the next batch.**
  The primitives exist in `pkg/drunmcp` (fork/rollback/diff/commit) and in the
  control plane (approvals, `fan_out_ceiling`), but their member-facing surfaces
  are not yet in the console.
- **`GET /me/agents` is a named dependency.** The console prefers it and falls
  back to `GET /agents`; the richer per-member entitlement view depends on that
  endpoint being present (ADR-0004 §3).
- **Sessions are short-lived** (~60 min default, ADR-0003 §4): a long task
  re-launches rather than renewing in place.

---

## 7. Quick start

```bash
# 1. Check prerequisites and print the ordered bring-up steps.
scripts/demo/member-console.sh

# 2. Bring up the org stack (see docs/RUNBOOK.md for the full reference).
cp deploy/docker/.env.app.example deploy/docker/.env.app   # first run only
docker compose --env-file deploy/docker/.env.app \
  -f deploy/docker/docker-compose.app.yml up --build -d

# 3. Get an org and an agent you are entitled to launch. Either sign up your
#    organization in the admin UI (http://localhost:8082) — you become Owner —
#    and create an agent, OR load the OPTIONAL demo seed for a ready-made demo
#    org (idempotent; a shortcut, not required):
docker compose --env-file deploy/docker/.env.app \
  -f deploy/docker/docker-compose.app.yml \
  exec -T postgres psql -U drundash -d drundash < deploy/seed/seed.sql

# 4. (For the conversational run, once it lands) start your local drun engine
#    so http://127.0.0.1:7273/mcp is reachable.

# 5. Start the member console and open it.
go run ./cmd/member-console      # serves http://localhost:8083
```

Then open **http://localhost:8083**, complete the device-flow login by approving
it in the admin UI (**http://localhost:8082**), pick an entitled agent, and
launch a governed session.
