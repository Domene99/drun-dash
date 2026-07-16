# internal/memberui

The **member console**: a small, server-rendered web app (Go + templ + HTMX) an
organization member runs on their own workstation. It is the second frontend
(design §6.3): sign-in, entitled-agent launch, and a **chat-first conversational
run view** where a natural-language directive becomes governed agent actions
(ADR-0004, ADR-0007). It renders on the shared design system
[`internal/uikit`](../uikit) (ADR-0005) — the same tokens, shell, and components
the admin dashboard uses — so the two surfaces cannot drift.

It is a separate binary (`cmd/member-console`) from the control plane and never
imports control-plane packages — it talks to the control plane over HTTP through
[`Client`](client.go), sending the member's session token as a Bearer
credential. It *also* talks locally to the drun engine via
[`pkg/drunmcp`](../../pkg/drunmcp) to run agent tool calls in a sandbox on the
member's machine — private code and its execution never leave the box (residency,
ADR-0004 §3). The same server is reused verbatim by the web binary
(`cmd/member-console`) and, per ADR-0007, by the desktop app that wraps it in a
native webview window.

## What it does

- **Sign in (email/password, primary local path — ADR-0008 §5).** `GET /login`
  leads with a password form (**org slug + email + password**). Submitting it
  `POST`s to `/login`, which calls the client's `Login` → `POST /auth/login`;
  on success the minted session JWT is stored in the same secure, HttpOnly
  `dd_member_session` cookie and the member is redirected to the home/agent
  picker. Every failure — unknown org, unknown email, wrong password, inactive
  member — is the control plane's single generic `401`, surfaced as one readable
  "invalid credentials" message that reveals nothing about which field was wrong
  (the org/email are echoed back so they need not be retyped; the password is
  not).
- **Sign in (device flow, RFC 8628 — secondary, for OIDC/SSO orgs).** The login
  page also starts the device flow: `GET /login` calls `POST /auth/device/code`,
  shows the `user_code` and a verification link, and stashes the `device_code`
  in a transient HttpOnly cookie. An HTMX poller hits `GET /login/poll`, which
  calls `POST /auth/device/token`; on approval the minted control-plane session
  JWT is stored in the same secure, HttpOnly cookie and HTMX navigates to the
  home page.
- **Agent picker (chat-first).** `GET /` is the landing surface: the agents the
  member may launch (prefers `GET /me/agents`, falls back to `GET /agents`),
  over the prior conversations they can reopen. The experience is "pick an agent
  and chat," not a bare table.
- **Launch / terminate.** `POST /sessions` launches the chosen agent
  (`POST /sessions/launch`) and **drops the member straight into the
  conversation** (an HTMX `HX-Redirect` to the run view); `POST
  /sessions/{id}/terminate` ends it, revoking the grant and closing the bound
  drun session. The launch `proxy_token` and `policy_bundle` are held
  **server-side** and never rendered to the page.
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
  - **Checkpoint timeline (§6.3).** `GET /sessions/{id}/timeline` renders the
    bound session's checkpoint/fork tree from `drunmcp.SessionHistory` /
    `SessionTree` (id, label, parent, file delta, current head, forks). The run
    view polls it (HTMX `hx-trigger`), so the checkpoints `agentrun` creates
    before risky steps appear as the run proceeds. A poll that lands mid-turn
    (the sequential drun client is busy) shows a "run in progress" note and
    refreshes once the turn settles.
  - **Diff-before-commit (§6.3) — the host-file-safety guarantee.** Nothing the
    agent did in the sandbox reaches the member's host files without an explicit
    member approval. `GET /sessions/{id}/diff` renders the pending
    `drunmcp.SessionDiff` (mounted baseline → current) with **Approve** /
    **Reject** actions; `POST /sessions/{id}/commit` is the only path that calls
    `drunmcp.SessionCommit`, and only on that explicit member action;
    `POST /sessions/{id}/reject` commits nothing. The agent loop is never offered
    `session_commit`/`session_export`/`session_snapshot`, and the tool executor
    refuses them even if an escalation is (erroneously) approved.
  - **Escalations, approve/resume (§6).** A policy-gated tool call is blocked and
    raised as `POST /approvals` (structure-only context), shown as a pending
    "waiting for approval" step in the chat, and paused — the action does not run
    and no result is fabricated. The console polls the escalation queue
    (`GET /approvals`, matched by id via `Client.GetApprovalStatus`), bounded by
    `MEMBER_CONSOLE_APPROVAL_TIMEOUT`: on **approved** it resumes and executes the
    blocked tool, feeding its real result back to the model; on
    **denied/expired/timeout** it feeds a governed denial so the model adapts. A
    proxy `401/403` likewise raises an approval. Admins decide in the admin UI
    (`POST /approvals/{id}/decision`).
- **Health.** `GET /healthz` returns a static `{"status":"ok"}`.
- **Design-system assets.** `GET /uikit/*` serves the shared `internal/uikit`
  stylesheet and theme script (content-hashed for cache-busting), so both the web
  binary and the desktop app render on one design system.

> **No matrix.** The member surface is chat-first, so the matrix fan-out view was
> removed (ADR-0007 §4): the `/matrix` routes, `matrix.go`, and the matrix
> templates are gone. The `internal/matrix` engine is **untouched** and still
> powers the CLI (`internal/cli/matrix.go`); only the member console's view of it
> was dropped. (Matrix's scorer is numeric, a poor fit for a text-producing model
> turn — removing it from the member surface resolves that mismatch by removal.)

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
| `MEMBER_CONSOLE_APPROVAL_TIMEOUT`       | `5m` | How long a blocked step waits for a decision before falling closed (denied) |
| `MEMBER_CONSOLE_APPROVAL_POLL_INTERVAL` | `3s` | How often to poll the escalation queue while waiting |

> Naming note: the console reads `DRUN_MCP_ENDPOINT`; `pkg/drunmcp`'s integration
> tests use `DRUNMCP_ENDPOINT`. Reconciling the two is a minor follow-up
> (ADR-0004).

Secure cookies are enabled only in production (HTTPS).

## Views

The member views compose the shared design system, [`internal/uikit`](../uikit)
(ADR-0005): `Layout` is the member "minimal" variant of `uikit.Shell` (top bar,
no admin sidebar — the conversation is the surface), and the pages are built from
uikit's `Card`, `Table`, `Button`, `Badge`, `Alert`, `EmptyState`, and `IDChip`.
The only member-specific styling is the small conversation stylesheet in
`views/styles.go` (transcript bubbles, tool-step chips, the diff/timeline
panels), layered on uikit's tokens so it themes with the rest — the member app
adds no design tokens and no bespoke component CSS.

The `*.templ` files under `views/` are the source of truth; the committed
`*_templ.go` files are generated with `templ generate`. The conversational run
view (`views/run.go`) and the dynamic run-view panels (`views/console_views.go`
— checkpoint timeline, diff review) are the exception: they are authored directly
as Go `templ.Component`s because they are HTMX fragments (and the SSE stream
needs string-returning renderers where each step is one `data:` line), which
templ's component-only output does not express. Edit the `.templ` files and
regenerate — never hand-edit the generated Go; `run.go` and `console_views.go`
are hand-authored and edited directly.
