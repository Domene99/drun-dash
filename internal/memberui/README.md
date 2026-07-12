# internal/memberui

The **member console**: a small, server-rendered web app (Go + templ + HTMX) an
organization member runs on their own workstation. It is the second frontend
(design §6.3): sign-in, entitled-agent launch, and a **conversational run view**
where a natural-language directive becomes governed agent actions (ADR-0004).

It is a separate binary (`cmd/member-console`) from the control plane and never
imports control-plane packages — it talks to the control plane over HTTP through
[`Client`](client.go), sending the member's session token as a Bearer
credential. It *also* talks locally to the drun engine via
[`pkg/drunmcp`](../../pkg/drunmcp) to run agent tool calls in a sandbox on the
member's machine — private code and its execution never leave the box (residency,
ADR-0004 §3). It deliberately mirrors `internal/ui` (the admin UI) without
importing it.

## What it does

- **Sign in (device flow, RFC 8628).** `GET /login` calls
  `POST /auth/device/code`, shows the `user_code` and a verification link, and
  stashes the `device_code` in a transient HttpOnly cookie. An HTMX poller hits
  `GET /login/poll`, which calls `POST /auth/device/token`; on approval the
  minted control-plane session JWT is stored in a secure, HttpOnly cookie and
  HTMX navigates to the home page.
- **Entitled agents.** `GET /` lists the agents the member may launch (prefers
  `GET /me/agents`, falls back to `GET /agents`).
- **Launch / terminate.** `POST /sessions` launches the chosen agent
  (`POST /sessions/launch`); `POST /sessions/{id}/terminate` ends it, revoking
  the grant and closing the bound drun session. The launch `proxy_token` and
  `policy_bundle` are held **server-side** and never rendered to the page.
- **Conversational run view (ADR-0004 §3–§5).** `GET /sessions/{id}` renders the
  run view for a launched session:
  - **Bind a drun session.** On first entry the console verifies the policy
    bundle fail-closed (`pkg/policy` load + freshness + content-hash — signature
    verification awaits org signing-key distribution), then
    `drunmcp.NewClient(DRUN_MCP_ENDPOINT)` → `Initialize` (the liveness probe;
    drun-mcp has no `/health`) → `CreateSession`. If the engine is unreachable or
    the bundle is rejected, the view shows a clear error and **withholds the
    chat** — no verified bundle / no engine, no run.
  - **Tool set.** `drunmcp.ListTools` → an `agentrun.ToolSpec` per tool, with the
    `session_id` parameter stripped from the schema (the console injects the
    bound session id at execution) and console-owned lifecycle tools
    (`create_session`, `session_close`, `session_restore`) dropped. If the policy
    bundle carries an `allowed_tools` list the set is intersected with it;
    otherwise **the full discovered drun tool set is offered** — the drun engine
    and the governed proxy still enforce policy at execution (documented choice,
    ADR-0004 §4).
  - **Run a turn (streamed).** Submitting a directive (`POST
    /sessions/{id}/turn`) stages it and returns a transcript fragment whose
    assistant bubble opens a Server-Sent Events stream (`GET
    /sessions/{id}/stream`). The stream drives `agentrun.Run` with the session's
    `proxy_base_url`/`proxy_token` and `MEMBER_CONSOLE_MODEL`, executing each tool
    call in the bound drun session and emitting one SSE `step` event per loop
    step, then `done`. A `ToolExecutor` adapter dispatches tool calls to the
    matching `drunmcp` helper.
  - **Trajectory (structure-only, §5).** Each turn emits one structure-class
    `TrajectoryEvent` per step to `POST /sessions/{id}/trajectory` — tool name,
    value type, byte size; **never** raw commands, file contents, or model text.
    The batch posts off the member's critical path (idempotent by `event_id`).
  - **Escalations (§6).** A tool call outside the offered set is blocked; the
    console raises `POST /approvals` (structure-only context) and fails closed —
    the blocked action never runs and the model gets a governed denial. A proxy
    `401/403` likewise raises an approval. Live poll-to-approve-then-resume is
    future work.
- **Health.** `GET /healthz` returns a static `{"status":"ok"}`.

### Known limitations

- **Per-turn context.** `agentrun.Run` seeds each turn from the directive alone,
  so the model does not carry conversation memory across directives yet (the
  transcript still accumulates visually). Cross-turn history awaits an
  `agentrun` enhancement.
- **Policy-bundle signature.** Verified structurally, for freshness, and by
  content hash; signature verification is wired once org signing-key
  distribution lands (ADR-0003).
- **Engine config.** The console consumes the verified bundle but does not push a
  compiled `config.toml` to the drun engine (there is no drun-mcp tool for it);
  the engine enforces its own provisioned config, and the proxy enforces at
  model-call time.

## Configuration

Read by `cmd/member-console` from the environment (see that command's doc
comment):

| Variable                    | Default                        | Purpose                                  |
| --------------------------- | ------------------------------ | ---------------------------------------- |
| `MEMBER_CONSOLE_HTTP_ADDR`  | `:8083`                        | Console listen address                   |
| `CONTROL_PLANE_BASE_URL`    | `http://localhost:8080`        | Control-plane base URL                   |
| `MEMBER_CONSOLE_CLIENT_ID`  | `drun-dash-member-console`     | Device-flow `client_id`                  |
| `MEMBER_CONSOLE_MODEL`      | _(unset)_                      | Model id passed to the proxy (runs need it) |
| `DRUN_MCP_ENDPOINT`         | `http://127.0.0.1:7273/mcp`    | Local drun engine endpoint               |

> Naming note: the console reads `DRUN_MCP_ENDPOINT`; `pkg/drunmcp`'s integration
> tests use `DRUNMCP_ENDPOINT`. Reconciling the two is a minor follow-up
> (ADR-0004).

Secure cookies are enabled only in production (HTTPS).

## Views

The `*.templ` files under `views/` are the source of truth; the committed
`*_templ.go` files are generated with `templ generate`. The conversational run
view (`views/run.go`) is the exception: it is authored directly as Go
`templ.Component`s because it needs string-returning fragment renderers for the
SSE stream (each step is one `data:` line), which templ's component-only output
does not express. Edit the `.templ` files and regenerate — never hand-edit the
generated Go; `run.go` is hand-authored and edited directly.
