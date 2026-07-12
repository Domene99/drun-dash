# 0004 — Member conversational console architecture

- **Status:** Proposed
- **Date:** 2026-07-11
- **Deciders:** drun-dash member-console / control-plane / proxy work streams
- **Batch:** 17 (decision artifact; no code)

## Context

The platform ships exactly one web frontend today — the **admin dashboard**
(`cmd/admin-ui`, `internal/ui`), which is how administrators govern the system.
The design (§6.3) also calls for a second, member-facing surface: a
**conversational console** where an ordinary org member types a natural-language
directive ("upgrade this service to Go 1.22 and run the tests") and watches a
**governed agent** carry it out in a sandbox. That surface was never built. This
ADR **locks its architecture before implementation**, which will span several
later batches, so those batches share one answer for where the console runs, how
it authenticates, how a directive becomes governed tool calls, and how the run
is observed and escalated. It writes **no code** and changes neither
`openapi/v1.yaml` nor the migrations; where the contract and the code on `main`
genuinely disagree, it records the behavior and names an explicit follow-up,
exactly as [ADR-0002](0002-rest-api-conventions.md) and
[ADR-0003](0003-session-launch-and-credential-grant.md) do.

The single most consequential constraint is **data residency**. Per the design
(§3, §5.2) and `docs/security/threat-model.md`, the drun engine runs on the
**member's own workstation**; private code and its execution never leave that
machine. A hosted, multi-tenant console that reached into members' machines
would invert that model. So the console must be a **local client** — it runs on
the member's machine, drives a **local drun engine**, and reaches *out* to the
org's control plane and governed LLM proxy for exactly the three things that must
be centralized: identity, model access, and governance.

What already exists and constrains this decision:

- **`pkg/drunmcp` — the local sandbox client.** `NewClient(endpoint)`,
  `Initialize` (also the liveness probe — drun-mcp has **no** `/health`),
  `ListTools`, and the generic `CallTool(ctx, name, args) (*CallToolResult,
  error)`, plus typed helpers (`CreateSession`, `SessionBash`, `SessionMount`,
  `SessionFork`, `SessionRollback`, `SessionDiff`, `SessionCommit`,
  `SessionClose`, `SessionGetEnv`, `GetFetchAllowlist`) and a `CheckpointRef`
  (`AtCheckpoint`/`AtLabel`). It speaks MCP over **Streamable HTTP** to
  `DefaultEndpoint = http://127.0.0.1:7273/mcp` (port `7273` is fixed in drun
  today); the integration tests override it with `DRUNMCP_ENDPOINT`. A `Client`
  is **sequential** — "concurrent calls on one Client are not supported" because
  the JSON-RPC ids and transport session id are shared serially. `CallToolResult`
  exposes `Text()` and `Decode(v)`. This is how the console drives sandboxed
  actions and checkpoints.
- **Session launch — [ADR-0003](0003-session-launch-and-credential-grant.md).**
  `POST /sessions/launch` (`launchSession`) returns a `SessionLaunchResponse`:
  `session_id`, a signed `policy_bundle`, `proxy_base_url`, a scoped
  `proxy_token`, `fan_out_ceiling`, and `expires_at`. The engine verifies the
  bundle fail-closed (`pkg/policy.Verify`) before running anything. The
  **governed LLM proxy** (`internal/proxy`, `cmd/llm-proxy`) is
  **OpenAI-compatible**: it forwards `POST /v1/chat/completions` to the upstream,
  injecting the real master key server-side; every call presents
  `Authorization: Bearer <proxy_token>` and is brokered, metered, and
  budget-checked at the proxy. The laptop never holds a master key.
- **Entitled agents — `GET /me/agents`.** A sibling batch-17 issue adds an
  endpoint that lists the *caller's* entitled agents (the ones this member may
  launch). It is **not yet on `main`**; this ADR treats it as the contract the
  console consumes and names the dependency.
- **Trajectory — `POST /sessions/{sessionId}/trajectory`
  (`ingestTrajectoryEvents`).** Batch-ingests `TrajectoryEvent`s (`event_id` for
  idempotency, `type` e.g. `tool_call`, `occurred_at`, `field_class`,
  `parent_event_id` for nested/sub-agent runs, and per-field `fields[]`). Each
  field and event carries a `FieldClass` of **`structure`** (shape/metadata only,
  the default) or **`payload`** (raw value, "only honored when policy permits").
  Returns `202` `IngestAck`. `GET …/trajectory` (`listTrajectoryEvents`) reads
  them back; the admin UI already renders them.
- **Escalations — `POST /approvals` (`createApproval`).** Raises an approval
  request "when an agent action exceeds its policy boundary. Typically called by
  the LLM proxy or the CLI on a fail-closed decision." `ApprovalCreate` is
  `{session_id, action, reason?, context?}`; returns `201` `Approval` with
  `status ∈ {pending, approved, denied, expired}`. Admins decide via
  `POST /approvals/{approvalId}/decision` (`decideApproval`) in the admin UI,
  which already backs the escalation inbox.
- **Auth building blocks — [ADR-0001](0001-authentication-and-session-model.md).**
  The **device-authorization flow** (RFC 8628) is already built end to end:
  `internal/cli/login.go` is the polling client (env `DRUN_DASH_SERVER`, default
  `http://localhost:8080`, `client_id = drun-dash-cli`), `internal/ui` serves the
  browser **device-approval page** behind OIDC login (`POST
  /auth/device/approve`), and `internal/clitoken` persists the resulting session
  token to a **`0600` file** in a `0700` dir under `os.UserConfigDir()`
  (`DRUN_DASH_CONFIG_DIR` override), with `Token.Expired()` and an optional
  `RefreshToken`. Access tokens carry a **distinct audience** for the proxy vs
  the control plane.
- **The admin-ui pattern — `internal/ui`.** A **separate Go binary** that never
  imports control-plane packages and talks to it only over HTTP: **Go + templ +
  HTMX**, a `chi` router with auth middleware, `ui.New(client, logger,
  ui.WithOIDC(authr))`, `oidc.go` (authorization-code + PKCE), a tiny REST
  `Client` (`client.go`) aware of the flat ADR-0002 `Error` envelope, and
  HTMX-refreshed fragments (`/fragments/summary`). The stack (Go + templ + HTMX,
  server-rendered) is **locked** by `TECH_STACK.md`.

## Decision drivers

- **Data residency is the whole point.** Private code and its execution stay on
  the member's machine. The console must be a local client that drives a local
  drun engine; nothing about the member's workspace may be forced off-box. This
  single constraint decides the deployment shape and colors every other section.
- **Governance is non-negotiable and already centralized.** Every model call
  goes through the proxy (metered, budget-checked, policy-bound); every action
  runs inside a sandbox constrained by the verified policy bundle; anything the
  policy blocks becomes a visible approval, not a dead error. The console's job
  is to *route* through those existing rails, never around them.
- **Reuse `main`, invent nothing parallel.** The device flow, `internal/clitoken`,
  `pkg/drunmcp`, the launch/grant flow, the trajectory and approval endpoints,
  and the admin-ui templ+HTMX pattern all exist. The console composes them; it
  does not fork a second auth system, a second sandbox client, or a second web
  stack.
- **Fail-closed, like every other plane.** No verified bundle → no run. Proxy
  rejects a call → surface an approval, do not silently retry unbrokered. drun
  engine unreachable → clear error, never a fallback to an ungoverned local
  model. A blocked tool call pauses the turn; it does not get executed anyway.
- **Consistency across the console's own batches.** One rule each for engine
  discovery, auth/token refresh, the turn loop's tool set, trajectory emission,
  and escalation, so the binary, the turn loop, and the web layer compose without
  re-litigating any of it.
- **Honest contract gaps.** `GET /me/agents` is not yet on `main`; the OpenAI
  tool-calling wire shapes are not in `openapi/v1.yaml` (they are the upstream
  model API, not our REST contract). Name these explicitly rather than pretend
  the surface is already complete.

## Decision

### 1. Deployment shape — a local binary serving a localhost web UI

- **A new binary, `cmd/member-console`, backed by a new `internal/console`
  package**, mirroring the `cmd/admin-ui` ↔ `internal/ui` split. It is a
  **local client the member runs on their own workstation**, not a hosted
  service. Like `internal/ui` it is a **separate binary that does not import
  control-plane packages** and talks to the control plane only over HTTP; unlike
  `internal/ui` it *also* talks locally to the drun engine via `pkg/drunmcp`.
- **Presentation is the locked stack — Go + templ + HTMX, server-rendered** — so
  the console reuses the admin-ui pattern wholesale (`chi` router, auth
  middleware, templ views, HTMX fragments, a small REST `Client` aware of the
  ADR-0002 `Error` envelope). The member opens `http://127.0.0.1:<port>` in their
  browser and interacts with a chat surface; there is no SPA and no separate
  frontend build.
- **Why local, not hosted.** Residency (§3, §5.2, threat model): the drun engine
  and the member's private code live on the member's machine and must never be
  forced off it. A hosted console would either need inbound access to each
  member's machine (a far worse security posture) or would have to relocate
  execution into the cloud (breaking residency outright). A local binary serving
  localhost keeps **code and execution on-box** while still funnelling the three
  centralized concerns — identity, model access, governance — out to the org
  over HTTP. This is also operationally consistent with admin-ui (same stack, same
  client shape) and with the CLI (same machine, same token store).
- **Bind to loopback only.** The server listens on `127.0.0.1` (never `0.0.0.0`),
  so the UI is reachable only from the member's own machine — it is a personal,
  single-member surface, not a shared server. This is a hard default, not a knob.
- **Engine discovery.** The console locates the local drun engine through
  `pkg/drunmcp`. It reads an endpoint from **`DRUN_MCP_ENDPOINT`**, defaulting to
  the client's own **`drunmcp.DefaultEndpoint` (`http://127.0.0.1:7273/mcp`)** —
  the address drun serves today. At startup it **probes with `Initialize`**
  (there is no `/health`); if the engine is unreachable the console renders a
  clear "start your local drun engine" state and refuses to launch a run, rather
  than degrading to anything ungoverned. Because a `drunmcp.Client` is
  **sequential**, the console uses **one client per active agent turn/session**
  and never shares a single client across concurrent turns (see §4).

  > Naming note: the environment variable is `DRUN_MCP_ENDPOINT` (the console's
  > own knob); `pkg/drunmcp`'s existing integration-test override is
  > `DRUNMCP_ENDPOINT`. The console reads `DRUN_MCP_ENDPOINT` and passes the
  > resolved value to `NewClient`; aligning the two names is a minor follow-up
  > for the implementing batch, not a blocker.

### 2. Authentication — reuse the device flow and `internal/clitoken`

- **Authenticate the member with the built device-authorization flow (RFC
  8628), not a new localhost OIDC callback.** The console is a local client with
  no server-side session and no registered redirect origin, which is *exactly*
  the case RFC 8628 exists for — and the flow is already implemented end to end
  (`internal/cli/login.go` client, `internal/ui` approval page, control-plane
  endpoints). On first use the console runs the same three steps the CLI does:
  `POST /auth/device/code`, show the member the `user_code` +
  `verification_uri` (and open it in their browser), and `POST
  /auth/device/token` until approval, honoring `authorization_pending` /
  `slow_down` per ADR-0001 §2. The member approves in the admin UI's
  device-approval page (behind OIDC), which binds the grant to their identity.
- **Store and refresh the token via `internal/clitoken`** — the same `0600`-file
  store the CLI uses (`os.UserConfigDir()/drun-dash/credentials.json`,
  `DRUN_DASH_CONFIG_DIR` override). The console loads the token at startup,
  checks `Token.Expired()`, and uses the optional `RefreshToken` to refresh
  before expiry (ADR-0001 §3); a missing or unrefreshable token drops the member
  back into the device flow. **The console shares the CLI's credential store** —
  logging in via the CLI logs in the console and vice versa — because it is the
  same user session token for the same control plane on the same machine.
- **The session token is a control-plane credential only.** It authenticates
  `GET /me/agents`, `POST /sessions/launch`, trajectory, and approval calls. It
  is **never** the credential for model calls: those use the launch-issued
  `proxy_token` (§4), which carries the proxy audience (ADR-0001 §3, ADR-0003
  §3). The two are not interchangeable, by audience.
- **Why device flow over a localhost OIDC callback.** A loopback
  authorization-code callback (à la admin-ui `oidc.go`) is possible, but it would
  make the *console* an OIDC client with its own registered redirect URI and PKCE
  handling, duplicating a flow the platform deliberately routed through the admin
  UI + device grant for local clients. The device flow needs no registered
  redirect, no per-console client registration, and reuses the existing approval
  page and token store unchanged. Reserved as a future enhancement if a fully
  browserless UX is ever wanted.

### 3. Agent selection and governed session launch

Before any turn runs, the console establishes a **governed session**:

1. **List entitled agents** with `GET /me/agents` (the caller's Bearer token)
   and present them for selection — the member can only run what they are
   entitled to; the console never invents an agent id. (This endpoint is the
   named batch-17 dependency; until it lands the console cannot populate the
   picker, so it is a hard prerequisite, not a nice-to-have.)
2. **Launch** the chosen agent with `POST /sessions/launch` (ADR-0003), yielding
   `session_id`, the signed `policy_bundle`, `proxy_base_url`, the scoped
   `proxy_token`, `fan_out_ceiling`, and `expires_at`.
3. **Verify the bundle fail-closed** with `pkg/policy.Verify` (structure,
   freshness, `content_hash`, Ed25519 signature) and compile `DrunConfig.ToTOML()`
   into the **local drun engine's** config so the sandbox is constrained by the
   org's policy *before* the first action runs. Any verification failure aborts
   the session with a clear error — no bundle, no run (ADR-0003 §2).
4. **Create the sandbox session** in the local engine via
   `drunmcp.CreateSession`, and hold `session_id` (control-plane) alongside the
   drun session id for the duration of the turn loop.

The control-plane `session_id` is the correlation key for **everything
downstream** — proxy calls (via `proxy_token`, which the proxy maps back to the
session), trajectory ingestion, and approvals all reference it. When the run
ends (or the member closes it) the console calls `POST
/sessions/{sessionId}/terminate`, which revokes the grant (ADR-0003 §3), and
closes the drun session (`drunmcp.SessionClose`). Sessions are **short-lived**
(ADR-0003 §4, ~60 min default): a long task re-launches rather than renewing.

### 4. The agent turn loop (the core)

A natural-language directive becomes governed actions through a **tool-calling
loop** the console drives locally. The model reasons *centrally* (through the
governed proxy); the tools execute *locally* (in the drun sandbox). Per turn:

1. **Seed the conversation** with the member's directive plus a **tool schema**
   describing the sandbox tools the model may call (§4, tool set below), and any
   prior turn history.
2. **Call the model through the governed proxy** — an OpenAI-compatible
   `POST {proxy_base_url}/v1/chat/completions` with
   `Authorization: Bearer <proxy_token>` and the tool schema. Because it routes
   through the proxy, **every** model call is brokered, metered, and
   budget-checked; the console never calls a model endpoint directly and never
   holds a master key.
3. **Interpret the response.** If the model returns a final answer, the turn is
   done and the console renders it. If it returns **tool calls**, go to (4).
4. **Execute each tool call locally in the drun sandbox** via `pkg/drunmcp`
   (`CallTool`/typed helpers) against the session's drun session id. Capture the
   result (`CallToolResult.Text()` / `Decode`).
5. **Feed results back** as tool-result messages and **loop to (2)** until the
   model produces a final answer, a bound is hit (see below), or an action is
   blocked (§6).

- **The tool set is policy-bound and drun-native.** The tools offered to the
  model are the drun sandbox tools exposed by `pkg/drunmcp`, discovered at
  session start with `ListTools` and then **intersected with the verified policy
  bundle's `allowed_tools`** — the model is only ever offered tools the org's
  policy permits for this agent. The core set is the session/file/exec/checkpoint
  tools: `session_bash`, `session_read_file`, `session_write_file`,
  `session_fork`, `session_checkpoint`, `session_diff`, `session_commit`,
  `session_history`, `session_tree`, and peers. This ADR fixes **that** the tool
  set is `ListTools ∩ policy.allowed_tools` and is drun-native; the exact
  per-tool JSON schema presented to the model is the implementing batch's, with
  the constraint that it faithfully mirror each drun tool's `inputSchema`.
- **Checkpoint the work.** Because the tool set includes drun's checkpoint/fork
  primitives, the loop **checkpoints before risky steps** so a bad step can be
  rolled back (`session_rollback` / `session_fork`) rather than corrupting the
  workspace — the console surfaces these as reviewable steps in the UI.
- **One engine client per turn; bound the loop.** Since a `drunmcp.Client` is
  **sequential**, each active turn owns its own client and executes its tool
  calls serially; concurrent turns/sessions get their own clients. The loop is
  **bounded** — a max iteration/tool-call count and the session's `expires_at` /
  `fan_out_ceiling` — so a stuck model cannot spin unmetered forever; on any
  bound it stops and reports rather than looping silently.
- **Fail-closed at every hop.** Proxy `401`/`403` (expired/blocked grant) →
  surface, don't retry unbrokered. drun engine unreachable mid-turn → stop with a
  clear error, never fall back to an ungoverned local model. Policy-blocked tool
  call → §6, not a swallowed error.

### 5. Trajectory emission — structure by default

As the loop runs, the console emits **structure-only trajectory events** to
`POST /sessions/{sessionId}/trajectory` so admins and auditors can see *what the
agent did* without the console shipping the member's private payloads off-box.

- **Emit one `TrajectoryEvent` per meaningful step** — model call, each tool
  call, tool result, turn boundary — with a client-generated `event_id` for
  idempotent retry, a `type` (e.g. `tool_call`), `occurred_at`, and
  `parent_event_id` to nest tool calls under their turn (and sub-agent
  trajectories under their parent when fan-out is used).
- **`field_class: structure` is the default, and the residency-preserving one.**
  Fields carry their **shape and metadata** — tool name, `value_type`,
  `byte_size`, exit status — but **not** raw file contents, commands, or model
  text. A field is emitted as **`payload`** (raw value) **only** when the
  verified policy explicitly permits capture of that field (the contract:
  "recorded only for fields explicitly marked `field_class: payload` and
  permitted by the session's policy"). The console **defaults every field to
  `structure`** and promotes to `payload` solely under policy — never on its own
  initiative. This makes governance observable while keeping private code on the
  member's machine, exactly matching the residency posture.
- **Best-effort, non-blocking, batched.** Trajectory ingestion returns `202`
  `IngestAck`; the console batches events and emits them **off the critical
  path** so telemetry never stalls the member's run. A failed post is retried
  (idempotent by `event_id`); it does **not** abort the turn. Emitting
  trajectory is observability, not a governance gate — the governance gates are
  the verified bundle (§3), the proxy (§4), and approvals (§6).

### 6. Escalations — policy-blocked actions become approvals, not dead errors

When an action the model wants exceeds the policy boundary, the console turns it
into a **visible approval request**, so a blocked step becomes a human decision
rather than a silent failure or a hard error the member cannot act on.

- **Where a block surfaces.** Two places, both fail-closed: (a) the **proxy**
  rejects a model call that exceeds policy/budget (a `4xx` fail-closed decision,
  per ADR-0003 §3 and the `createApproval` note that the proxy "typically" raises
  these); and (b) a **tool call** the model proposes falls outside the session's
  `allowed_tools` or the drun engine refuses it under the compiled config.
- **Raise an approval.** On either, the console (when it is the party holding the
  member context) issues `POST /approvals` with `ApprovalCreate = {session_id,
  action, reason, context}`, where `action` names the blocked operation,
  `reason` explains why it exceeded policy, and `context` carries **structure-only**
  decision aids consistent with §5 (never raw private payloads). The response is
  a `pending` `Approval`.
- **Surface it in the UI and pause the step, don't kill the turn.** The console
  shows the pending escalation in the chat surface (an HTMX-refreshed fragment,
  like admin-ui's live fragments) and **pauses that step** — the loop does not
  execute the blocked action and does not fabricate a result for the model.
  Admins decide in the **admin UI** via `POST /approvals/{approvalId}/decision`
  (`decideApproval`); the console polls / refreshes the approval and, on
  `approved`, proceeds with the action, or on `denied`/`expired` feeds a
  governed "action denied" result back to the model so it can adapt. Nothing
  blocked ever runs un-approved.

### 7. What the console does *not* decide or hold

To keep the residency and custody invariants crisp:

- **No master model key, ever.** Model access is the launch-issued `proxy_token`
  against `proxy_base_url`; the real upstream key lives only in the proxy
  (ADR-0003). The console holds only a short-lived, scoped, revocable grant.
- **No policy authoring or enforcement logic.** The console **consumes** a signed
  bundle and lets the drun engine enforce the compiled config; it never decides
  what is allowed. Governance decisions live in the control plane and the policy
  bundle.
- **No private code off-box.** Code and execution stay in the local drun engine;
  only **structure-only** trajectory and **structure-only** approval context
  leave the machine, and raw payloads only under explicit policy.

## Consequences

**Positive**

- One agreed shape for the member surface: the binary, the turn loop, and the web
  layer implement against the same deployment model, auth flow, launch/verify
  path, tool set rule, trajectory posture, and escalation flow — no
  re-litigation across the console's several batches.
- Residency holds by construction: private code and its execution never leave the
  member's machine; the console reaches out only for identity, model access, and
  governance, and emits only structure by default.
- Governance is inescapable: every model call is brokered/metered/budget-checked
  at the proxy, every action runs inside a policy-constrained sandbox, and every
  policy block becomes a visible approval — the console routes *through* the
  existing rails, never around them.
- Near-total reuse of `main`: the device flow, `internal/clitoken`, `pkg/drunmcp`,
  the ADR-0003 launch/grant/verify pipeline, the trajectory + approval endpoints,
  and the admin-ui templ+HTMX pattern are all composed, not reinvented.

**Negative / trade-offs**

- **A hard dependency on `GET /me/agents`**, which is not yet on `main` (a
  sibling batch-17 issue). Until it lands the console cannot populate the agent
  picker. Named as a prerequisite, not worked around.
- **The OpenAI tool-calling wire shapes are not in `openapi/v1.yaml`.** They are
  the upstream model API the proxy forwards, not part of our REST contract, so
  the console depends on the proxy faithfully passing through
  `/v1/chat/completions` tool-calling semantics. Recorded as a proxy-compat
  assumption, not a contract change here.
- **Sequential `drunmcp.Client` bounds intra-turn parallelism.** One client per
  turn executes tool calls serially; parallel *fan-out* needs multiple clients /
  sessions under the launch `fan_out_ceiling`. Accepted; the loop is serial per
  turn by design.
- **A second web binary to build and maintain** (`cmd/member-console` /
  `internal/console`) alongside admin-ui. Mitigated by sharing the locked stack
  and the admin-ui patterns, but it is genuinely new surface area.
- **The `DRUN_MCP_ENDPOINT` vs `DRUNMCP_ENDPOINT` naming split** is a small,
  named inconsistency to reconcile in implementation.

## Alternatives considered

- **Hosted, multi-tenant console** (a cloud web app members log into). Rejected:
  it breaks data residency — it would need inbound access to each member's
  machine or would relocate execution to the cloud, exactly what §3/§5.2 and the
  threat model forbid. The console must be local.
- **A native desktop / TUI app instead of a localhost web UI.** Rejected for v1:
  the stack is locked to Go + templ + HTMX server-rendered, and a localhost web
  UI reuses the entire admin-ui pattern (router, auth middleware, views, HTMX
  fragments, REST client) at near-zero incremental design cost. A richer native
  client is possible future work behind the same control-plane + drun seams.
- **Localhost OIDC authorization-code callback** (make the console its own OIDC
  client, like admin-ui). Rejected for v1: it duplicates a redirect/PKCE flow the
  platform deliberately routes through the admin UI + device grant for local
  clients, and needs per-console client registration. The device flow is built,
  needs no registered redirect, and reuses the approval page and token store
  unchanged. Kept as a future enhancement.
- **A separate console credential store** (its own token file). Rejected: it is
  the same user session token for the same control plane on the same machine;
  sharing `internal/clitoken` means one login serves CLI and console and
  revocation is uniform.
- **Console calls model providers directly** (holding a key, or bypassing the
  proxy). Rejected outright: it destroys metering, budget enforcement, and policy
  brokering and would put a master credential on the laptop — the exact thing the
  threat model and ADR-0003 forbid. All model calls go through `proxy_base_url`
  with the scoped `proxy_token`.
- **Ship full payloads in trajectory by default** (rich replay). Rejected: it
  would push private code and outputs off the member's machine, violating
  residency. Structure-by-default with policy-gated `payload` promotion is the
  contract and the residency-preserving choice.
- **Treat a policy-blocked action as a hard error** (fail the run). Rejected: the
  design requires escalations become approvals surfaced in the UI, so a member is
  never stuck at a dead error an admin could have unblocked. Blocked → `POST
  /approvals` → human decision → resume or governed denial.
- **A long-lived console session with in-place renewal.** Rejected for v1,
  consistent with ADR-0003 §4: sessions are short-lived and a long task
  re-launches; a renew endpoint is named future work there, not introduced here.

## Out of scope for this ADR (explicit)

- **Any OpenAPI or migration change.** `GET /me/agents` is delivered by its own
  batch-17 issue; the `DRUN_MCP_ENDPOINT`/`DRUNMCP_ENDPOINT` reconciliation and
  any console-specific endpoints are proposals for the implementing batches, not
  changes made here.
- **The concrete per-tool JSON schema** presented to the model, and the exact
  system-prompt / conversation-format details of the turn loop — owned by the
  implementing batch; this ADR fixes the loop's *shape* (governed proxy call →
  policy-bound drun tools → feed back → repeat) and the tool-set rule
  (`ListTools ∩ policy.allowed_tools`), not the field lists.
- **Multi-agent fan-out orchestration** beyond honoring `fan_out_ceiling` and
  nesting sub-agent trajectories under `parent_event_id` — the detailed fan-out
  scheduler is later work.
- **The exact trajectory batching / retry cadence and the approval polling
  interval** — implementation tuning, constrained only by "off the critical path,
  idempotent, non-blocking" (§5) and "pause the step, never fabricate a result"
  (§6).
- **OS-keychain token storage** — v1 reuses the `internal/clitoken` `0600` file;
  keychain is the same future enhancement ADR-0001 §4 names.
- **A browserless / fully-embedded auth UX** — v1 uses the device flow with the
  browser approval page; a localhost OIDC callback is named future work.
- **Signing-key management, budget-cap evaluation, and proxy internals** — owned
  by ADR-0001, ADR-0003, and their implementing batches; the console only
  consumes their outputs.

## References

- `openapi/v1.yaml` — `launchSession`/`terminateSession`,
  `ingestTrajectoryEvents`/`listTrajectoryEvents`, `createApproval`/
  `decideApproval`, `TrajectoryEvent`, `TrajectoryField`, `FieldClass`,
  `Approval`, `ApprovalCreate`, the OpenAI-compatible proxy `/v1/chat/completions`.
- `pkg/drunmcp` — `NewClient`, `Initialize`, `ListTools`, `CallTool`,
  `CallToolResult`, typed session helpers, `CheckpointRef`, `DefaultEndpoint`
  (`http://127.0.0.1:7273/mcp`), sequential-client constraint, `README.md`.
- `internal/clitoken` — `Token`, `Default`/`New`, `Save`/`Load`/`Clear`,
  `Token.Expired`, the `0600`-file store, `README.md`.
- `internal/cli/login.go` — the RFC 8628 device-flow client the console reuses.
- `internal/ui` — the admin-ui templ+HTMX pattern: `server.go` (`chi` router,
  auth middleware, HTMX fragments), `oidc.go`, `client.go`, the device-approval
  page, `README.md`.
- `internal/proxy`, `cmd/llm-proxy` — the OpenAI-compatible governed broker.
- [ADR-0001](0001-authentication-and-session-model.md) — device flow, JWT
  session token, distinct audiences, `internal/clitoken` storage, fail-closed.
- [ADR-0002](0002-rest-api-conventions.md) — org scoping, `Error` envelope,
  naming contract↔schema gaps as follow-ups.
- [ADR-0003](0003-session-launch-and-credential-grant.md) — session launch,
  signed policy bundle + fail-closed verify, scoped `proxy_token`, termination,
  short-lived sessions, metering.
- `docs/security/threat-model.md` — residency, semi-trusted laptop, scoped
  short-lived tokens, prompt revocation, fail-closed offline.
- `docs/ARCHITECTURE.md`, `docs/TECH_STACK.md` — planes and the locked
  Go + templ + HTMX stack. Design §3, §5.2, §6.3 — residency and the
  member-facing agent interface.
