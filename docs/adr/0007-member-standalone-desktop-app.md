# 0007 — Member standalone desktop app (webview shell, chat-first, drop matrix)

- **Status:** Proposed
- **Date:** 2026-07-15
- **Deciders:** drun-dash member-console / control-plane / proxy work streams
- **Batch:** 21 (decision artifact; no code)

> **Numbering note.** The tracking issue calls this "ADR 0007" and refers to a
> "member shell (ADR-0005)". On `main` the member conversational console is
> [**ADR-0004**](0004-member-conversational-console-architecture.md) and the next
> free sequence number is **0005**; there are no ADRs 0005 or 0006 and nothing
> reserves them. Per the [README](README.md) convention (contiguous
> zero-padded sequence, numbers never reused, no gaps) this ADR is **0005** and
> every "member shell" reference below points at ADR-0004.

## Context

The member surface shipped over batches 17–20 as a **localhost web app**: the
member runs `cmd/member-console` on their own workstation, opens
`http://127.0.0.1:<port>` (default `:8083`) in their **browser**, signs in with
the device flow, picks an entitled agent, and drives a governed conversational
run — model calls through the proxy, tool calls in the local drun sandbox,
streamed live, with a checkpoint timeline and diff-before-commit. That whole
surface is `internal/memberui` (Go + templ + HTMX + `chi`), exactly the shape
[ADR-0004](0004-member-conversational-console-architecture.md) locked.

ADR-0004 §1 chose a **localhost web UI** and, in its Alternatives, explicitly
**deferred** "a native desktop / TUI app" for v1 — reusing the admin-ui pattern
at near-zero incremental cost was the win, and a native client was named as
"possible future work behind the same control-plane + drun seams." The owner has
now made that future call: **the member app should be a standalone desktop
application, not a browser tab** — while still running **entirely locally** (the
member's code and its execution never leave their machine; the drun engine runs
locally). The admin dashboard stays a web app. This ADR **revises only
ADR-0004 §1 (deployment shape)** — the window replaces the browser tab — and
**reaffirms ADR-0004 §§2–7 unchanged** (auth, launch/verify, the governed turn
loop, structure-only trajectory, and escalations-as-approvals all stand). It
also locks two owner decisions that ADR-0004 did not face: the member experience
is **chat-first** and the **matrix UI is removed** from the member surface.

This ADR writes **no code**. It touches neither `openapi/v1.yaml` nor the
migrations, and — per the batch constraint — changes no Go source, only
`docs/adr/`. Where the desired end state and the code on `main` differ, it
records the target and names an explicit follow-up, exactly as ADR-0002,
ADR-0003, and ADR-0004 do.

What already exists and constrains this decision:

- **`internal/memberui` — the entire member surface, server-rendered.**
  A `chi` router with device-flow sign-in (`/login`, `/login/poll`), an
  entitled-agent home (`GET /`, preferring `GET /me/agents`), launch/terminate
  (`POST /sessions`, `POST /sessions/{id}/terminate`), the conversational run
  view (`GET /sessions/{id}`, SSE-streamed turns via `internal/agentrun`), the
  checkpoint timeline and diff-before-commit (`checkpoints.go`, `commit.go`),
  escalation approve/resume (`approvals.go`), structure-only trajectory
  (`trajectory.go`), and the matrix view (`matrix.go`). It **never imports
  control-plane packages** — it speaks HTTP through `client.go` with the member's
  session JWT as a Bearer credential — and talks locally to the drun engine via
  `pkg/drunmcp` (`engine.go`). The launch `proxy_token` and `policy_bundle` are
  held **server-side** and never rendered to the page.
- **`cmd/member-console` — the plain web binary.** Wires `internal/memberui`,
  binds `MEMBER_CONSOLE_HTTP_ADDR` (default `:8083`), reads
  `CONTROL_PLANE_BASE_URL`, `MEMBER_CONSOLE_CLIENT_ID`, `MEMBER_CONSOLE_MODEL`,
  `DRUN_MCP_ENDPOINT` (default `drunmcp.DefaultEndpoint`,
  `http://127.0.0.1:7273/mcp`), and the approval timeout/poll knobs. Pure Go, no
  cgo; CI builds it with plain `go build`.
- **Session auth — device flow + cookie session.** The console signs in through
  the RFC 8628 device-authorization flow (ADR-0001, ADR-0004 §2): request a
  device+user code, show the `user_code` and `verification_uri`, and poll
  `POST /auth/device/token` until the member approves it in the admin UI's
  device-approval page. The minted control-plane session JWT is stored in a
  **secure, HttpOnly cookie** and forwarded as the Bearer credential.
  `internal/clitoken` is the CLI's sibling `0600`-file store for the same token;
  both are merged and on `main`.
- **`internal/matrix` — the parallel matrix engine (design §6.3).** Fan a task
  across N governed sessions ("branches"), run each, **score every branch with a
  numeric objective function**, promote the winner, terminate all. Its scorer
  parses a branch's stdout as a `float64` (`parseScore`: last non-empty line →
  `strconv.ParseFloat`; a non-numeric line is `StatusScoreFailed`). It is
  imported by **two** callers today: `internal/cli/matrix.go` (the CLI) and
  `internal/memberui/matrix.go` (the member console's `/matrix` view, added in
  batch 20).
- **The webview toolchain — `github.com/webview/webview_go`.** A tiny cgo library
  that opens a **native OS webview window** over the platform web engine — system
  **WebKit** (`WKWebView`) on macOS, **WebView2** on Windows, **WebKitGTK** on
  Linux — and navigates it to a URL. Its API is `webview.New(debug)`,
  `SetTitle`, `SetSize`, `Navigate(url)`, `Run()` (blocks on the main OS thread
  until the window closes), and `Destroy()`. It embeds **no** browser engine and
  ships **no** JS framework; the page it renders is whatever the URL serves.
- **The locked stack — `docs/TECH_STACK.md`.** Go + templ + HTMX, server-rendered,
  no SPA, no bundler, no `node_modules`; "the **only** JS-adjacent dependency
  allowed is HTMX loaded from a CDN inside HTML templates." A webview pointed at
  the existing templ+HTMX server honors this verbatim — it is the same HTML in a
  native window, not a new frontend.
- **Residency — design §3, §5.2, `docs/security/threat-model.md`.** The drun
  engine and the member's private code live on the member's machine and must
  never be forced off it. A desktop window changes the chrome, not this
  invariant.

## Decision drivers

- **A real app, not a browser tab — without a second UI codebase.** The owner
  wants a standalone desktop application, but the locked stack and the entire
  member surface are already Go + templ + HTMX. The design must give the member a
  native window while **reusing `internal/memberui` verbatim** — no rewrite, no
  JS framework, no second frontend.
- **Local is still the whole point.** Residency is unchanged: the engine and the
  member's code stay on-box. "Desktop app" must not smuggle in any hosted
  component; it is a local shell over a local server driving a local engine.
- **Reuse `main`, invent nothing parallel.** The device flow, cookie session,
  `pkg/drunmcp`, the launch/verify pipeline, trajectory, approvals, and the whole
  run/checkpoint/diff surface exist. The desktop app **wraps** them; it does not
  fork a second auth system, a second sandbox client, or a second web layer.
- **Chat-first, one clear IA.** The member experience is agent picker → governed
  conversation, with history, checkpoints, and diff as supporting panels. The
  matrix — a batch-oriented, numeric-scored fan-out — does not belong on this
  surface and is removed from it.
- **Don't break CI or the plain build.** cgo + a platform web engine is a
  packaging-time toolchain, not something `go build` or CI should require. The
  plain web binary must keep building unchanged; the desktop app is an **optional
  local/release artifact**.
- **Fail-closed and honest gaps, like every plane.** Engine unreachable → a clear,
  friendly "start drun" state, never a fallback to anything ungoverned.
  Auto-launching the engine and non-macOS packaging are real follow-ups, named
  as such rather than pretended complete.

## Decision

### 1. Architecture — a native window over the in-process local server

- **A new binary, `cmd/member-app`**, is the standalone desktop application. It
  is a Go program that does two things in one process:
  1. **Runs the existing member-console HTTP server in-process**, bound to
     **`127.0.0.1:<random>`** (an ephemeral loopback port — ask the OS for
     `127.0.0.1:0` and read back the chosen port). This is the *same*
     `internal/memberui` server the web binary runs — same router, same device
     flow, same run view, same checkpoints/diff/approvals — constructed and
     started the identical way, just on a random loopback port instead of a
     fixed `:8083`.
  2. **Opens a native OS webview window** with `github.com/webview/webview_go`,
     sizes and titles it (e.g. "drun — member console"), and **navigates it to
     `http://127.0.0.1:<random>/`** — the address the in-process server just
     bound. `webview.Run()` blocks on the main OS thread for the window's life.
- **Reuse `internal/memberui` verbatim, inside a real app window.** The webview
  renders the existing templ+HTMX pages exactly as the browser does — HTMX from
  the CDN, SSE-streamed runs, HTMX-refreshed fragments — so there is **no
  browser chrome, no JS framework, and no second UI codebase**. This is why the
  desktop app costs almost nothing beyond the shell: the window is new; the UI is
  not.
- **Lifecycle: the window owns the process.** Start the server first, wait until
  it is accepting connections, then create and run the window. When the window
  closes (`Run()` returns, or `Destroy()`), **shut the in-process server down
  gracefully** (`http.Server.Shutdown` with a bounded timeout) and exit. No
  orphaned server, no lingering loopback listener.
- **Loopback only, ephemeral port, no shared surface.** Binding `127.0.0.1:0`
  keeps the server reachable **only from the member's own machine** and picks a
  fresh port per launch, so nothing collides with a separately-run
  `cmd/member-console` and no fixed port is exposed. This is the ADR-0004 §1
  "bind to loopback only" rule, tightened to an ephemeral port because the only
  intended client is the app's own window.
- **What does *not* change.** Everything ADR-0004 §§2–7 locked is reached through
  this window unchanged: device-flow auth (§2, §3 below), launch + fail-closed
  bundle verify + drun `CreateSession` (ADR-0004 §3), the governed turn loop
  (proxy model call → policy-bound drun tools → feed back → repeat, ADR-0004 §4),
  structure-only trajectory (ADR-0004 §5), and escalations-as-approvals
  (ADR-0004 §6). The desktop app is a **shell around `internal/memberui`**; it
  adds a window and subtracts nothing from governance.

### 2. drun engine — detect a local engine, guide if absent

- **Target a locally running drun engine** via `pkg/drunmcp`, reading
  **`DRUN_MCP_ENDPOINT`** and defaulting to `drunmcp.DefaultEndpoint`
  (`http://127.0.0.1:7273/mcp`) — unchanged from `cmd/member-console`. The engine
  runs on the member's machine; the app never reaches for a remote one.
- **Detect, don't assume.** drun-mcp has **no `/health`**; the app probes with
  `Initialize` (the liveness probe, ADR-0004 §1). This already happens per run in
  `internal/memberui` (`engine.go`), and the run view **withholds the chat** when
  the engine is unreachable. The desktop app adds a **friendly, app-level state**:
  if no engine is detected it shows a clear "drun isn't running" panel **with a
  short 'how to start drun' hint**, rather than a bare error, and refuses to
  launch a run — never degrading to anything ungoverned.
- **Auto-launch / bundle is a documented follow-up.** v1 only *detects* a running
  engine. Having the app **start** (or ship) the drun binary itself is deferred:
  it means locating or embedding a versioned `drun` binary, managing its
  lifecycle alongside the window, and materially larger packaging (the engine
  binary inside the `.app`, plus its own permissions/first-run story). Named here
  so a later batch can pick it up; not built now.

### 3. Auth — keep the device flow; open the browser for OIDC

- **Reuse the merged device flow unchanged** (ADR-0001, ADR-0004 §2). Even in a
  native window the app is a local client with no registered redirect origin —
  exactly the case RFC 8628 exists for. The app shows the **`user_code`** and
  **opens the `verification_uri` in the member's default browser** (the OS
  "open URL" affordance, distinct from the app's own webview), where the member
  completes OIDC login and approves the grant in the admin UI's device-approval
  page; the app **polls** `POST /auth/device/token` until approval, honoring
  `authorization_pending` / `slow_down`.
- **Session storage is as shipped.** The minted control-plane session JWT lives
  in the member-console server's **secure HttpOnly cookie** (and, for the CLI,
  the `internal/clitoken` `0600` file) — the desktop app inherits this because it
  runs the same server; it introduces no new credential store. The JWT is a
  **control-plane credential only**; model calls use the launch-issued
  `proxy_token` (ADR-0003, ADR-0004 §2), never this token.
- **Opening the verification URL in the default browser (not the app's own
  webview) is deliberate.** OIDC login belongs in the user's real browser, with
  its existing session, password manager, and the platform's registered redirect
  — not inside the app window. A **loopback OIDC authorization-code redirect** is
  a viable alternative (the app registers as its own OIDC client and catches the
  callback on a loopback port); it is noted as a future option, but v1 keeps the
  built device flow, which needs no per-app client registration and reuses the
  approval page and token handling verbatim.

### 4. Chat-first experience — and remove the matrix

- **The window's IA is the ADR-0004 member shell, chat-first:** an **agent
  picker** (the member's runnable agents from role-based access, `GET /me/agents`)
  → a **governed conversation** (the run view: model via the proxy, tools in the
  local drun sandbox, SSE-streamed), with **session history**, the **checkpoint
  timeline**, and **diff-before-commit** as supporting panels. This is precisely
  what `internal/memberui` already renders; the desktop app changes the frame,
  not the flow.
- **Remove the matrix UI from the member surface.** Drop the member console's
  matrix view — the `/matrix` routes (`GET`/`POST`), `internal/memberui/matrix.go`,
  its tests, and the matrix view templates — from the member app's IA. Matrix is
  a batch-oriented, numeric-scored fan-out; it is not the chat-first member
  experience and does not belong on this surface.
- **Fate of `internal/matrix`: keep the package, drop it from the member app.**
  The engine package stays because **`internal/cli/matrix.go` still uses it** —
  matrix remains a valid CLI/power-user workflow. Removing the package outright
  would break the CLI for no benefit; the decision is scoped to the **member
  surface**, where only the `internal/memberui` importer is removed. (If a later
  batch also retires the CLI matrix command, the package can be deleted then;
  that is out of scope here.)
- **The matrix scorer bug is resolved-by-removal on the member surface.** The
  matrix scorer is **numeric**: `parseScore` requires the score command's stdout
  to parse as a `float64`, and a non-numeric line yields `StatusScoreFailed`. On
  the member surface the branch work is a governed **model** turn whose natural
  output is **text**, so feeding that text to the numeric scorer mismatches by
  construction — the member matrix view could not reliably score model output.
  Removing the matrix from the member app **removes this failure mode from the
  member surface** by construction. (The CLI matrix keeps the numeric-scorer
  contract, where the caller supplies an explicit scoring command that *is*
  expected to print a number — the correct place for that contract.)

### 5. Packaging — macOS `.app` first; the plain binary keeps building

- **v1 target: macOS, a `.app` bundle**, with a documented build recipe in the
  binary's README (compile `cmd/member-app` with cgo enabled against system
  WebKit, assemble the `Foo.app/Contents/{MacOS,Info.plist,Resources}` layout,
  set the bundle identifier and icon; optionally codesign for local distribution).
  Windows (WebView2 + a `.exe`/installer) and Linux (WebKitGTK + a desktop entry)
  packaging are **named follow-ups**, not v1.
- **cgo/webview is a packaging-time toolchain, not a build-time requirement.**
  `cmd/member-app` needs cgo and the platform web engine's dev headers; that is a
  **local/release** concern. **CI keeps building the plain binary**
  (`cmd/member-console`) with ordinary `go build`, and the repo does not require
  the webview toolchain to compile, test, or ship the web surface. The desktop
  packaging step is optional and gated to where the toolchain exists.
- **Keep `cmd/member-console` as the headless/dev web binary.** It is the same
  `internal/memberui` server, buildable everywhere with no cgo — ideal for
  development, headless/remote use, and CI. `cmd/member-app` **reuses** it (runs
  that server in-process) rather than replacing it, so the two share one code
  path and one behavior; only the shell differs. Folding the web binary away
  would cost the cgo-free build and the headless entry point for no gain, so it
  **stays**.

### 6. What stays server-side / unchanged (the invariants)

The desktop app is a **shell around `internal/memberui`**; every governance
invariant from ADR-0001/0003/0004 is untouched:

- **No master model key, ever.** Model access is the launch-issued `proxy_token`
  against `proxy_base_url`; the upstream key lives only in the proxy. The app
  holds only a short-lived, scoped, revocable grant — server-side, never rendered.
- **Policy is verified and enforced as shipped.** The console loads and verifies
  the signed `policy_bundle` fail-closed before the first action, compiles it
  into the local drun engine, and lets the engine + proxy enforce it. The window
  authors no policy and enforces nothing itself.
- **Trajectory is structure-only by default** (ADR-0004 §5); **approvals** gate
  every policy-blocked step (ADR-0004 §6); **diff-before-commit** is exactly as
  shipped. None of these changes because the UI moved into a window.
- **Nothing new leaves the box.** The window is local, the server is local
  (loopback, ephemeral port), the engine is local. Only the same
  identity/model/governance traffic ADR-0004 already sends to the control plane
  and proxy leaves the machine — no more, no less.

### 7. What this ADR does not decide

- **The exact window ergonomics** — size/title/menu/icon specifics, multi-window
  or tabbed sessions, deep-link handling — are the implementing batch's, bounded
  only by §1 (loopback in-process server, window owns process lifecycle).
- **Auto-launching or bundling the drun engine** (§2) and **non-macOS packaging**
  (§5) are named follow-ups, not designed here.
- **A loopback OIDC authorization-code redirect** (§3) as an alternative to the
  device flow is reserved, not adopted.
- **Deleting `internal/matrix`** or retiring the **CLI** matrix command (§4) —
  only the member-surface importer is removed here; the package stays while the
  CLI uses it.
- **OS-keychain token storage** — inherited from ADR-0004/ADR-0001 as the same
  future enhancement; the app reuses the shipped cookie/`clitoken` storage.

## Consequences

**Positive**

- A genuine standalone desktop app **with zero new UI code**: the native window
  renders `internal/memberui` verbatim, so the entire run/checkpoint/diff/approval
  surface, the device flow, and the streamed turn loop work in the app exactly as
  in the browser. One UI, two shells.
- Residency and governance hold by construction — the app only adds a window over
  a **loopback, ephemeral-port** server driving a **local** engine; credential
  custody, the proxy, trajectory, and approvals are exactly as ADR-0004 shipped.
- The locked Go + templ + HTMX stack is honored, not bent: no SPA, no bundler, no
  JS framework — the webview is a frame around the same server-rendered HTML.
- CI and the plain build are untouched: `cmd/member-console` still builds and
  ships with ordinary `go build`; the cgo/webview toolchain is confined to an
  optional packaging step.
- The member IA gets simpler and more honest — chat-first, with the numeric-scored
  matrix (a poor fit for text-producing model turns) removed from the surface,
  taking its scorer mismatch with it.

**Negative / trade-offs**

- **A cgo dependency and a platform toolchain** enter the repo for `cmd/member-app`
  (system WebKit / WebView2 / WebKitGTK). Mitigated by keeping it off the default
  build and CI, but it is real new surface for whoever packages releases.
- **Per-platform packaging work.** v1 is macOS-only; Windows and Linux each need
  their own web engine and bundle format — named follow-ups, so members on those
  platforms use the plain web binary until then.
- **Two entry points to keep coherent** (`cmd/member-console` and
  `cmd/member-app`). Mitigated because the app **runs the same server** in-process
  rather than duplicating it, so behavior can't drift — but there are two `main`s.
- **v1 requires the member to start drun themselves.** The app only detects the
  engine; until auto-launch lands, "drun isn't running" is a guided dead-end, not
  a one-click fix.
- **Matrix disappears from the member UI.** Members who used the member-console
  matrix view lose it there; it remains available via the CLI. Accepted: matrix
  is not the chat-first member experience.

## Alternatives considered

- **Keep the browser tab (status quo, ADR-0004 §1).** Rejected by owner decision:
  a shared browser tab is not a standalone app, doesn't feel like a first-class
  member tool, and mixes the governed console with the member's ordinary browsing
  session. The window is the point.
- **Rewrite the member UI as a native/desktop framework** (a real GUI toolkit, or
  an Electron-style embedded app with a JS framework). Rejected: it forks a second
  UI codebase, violates the locked Go + templ + HTMX stack (and the "no bundler /
  no `node_modules`" rule), and throws away the shipped `internal/memberui`
  surface. The webview-over-local-server approach gets a native window while
  reusing that surface verbatim.
- **A different embedding library** (e.g. a heavier Chromium-embedding
  framework). Rejected for v1: `webview/webview_go` is tiny, uses the **system**
  web engine (no bundled browser, small binary), and only needs a URL — which is
  exactly what the in-process server provides. A bundled-Chromium approach adds
  hundreds of MB and its own update burden for no benefit here.
- **Bind the in-process server to a fixed port** (like `:8083`). Rejected: a fixed
  port can collide with a separately-run `cmd/member-console` and needlessly
  exposes a predictable local surface. `127.0.0.1:0` (ephemeral) is reachable only
  by the app's own window and never collides.
- **Fold `cmd/member-console` into the desktop app** (one binary). Rejected: it
  would drag the cgo/webview toolchain into every build and drop the cgo-free
  headless/dev/CI entry point. Keeping the plain binary and having the app reuse
  its server is strictly better.
- **Auto-launch or bundle the drun engine in v1.** Deferred, not rejected: it is
  desirable but materially enlarges packaging and adds an engine-lifecycle story;
  v1 detects and guides, and a follow-up can bundle.
- **A loopback OIDC authorization-code redirect instead of the device flow.**
  Reserved as future work: it makes the app its own OIDC client with a registered
  redirect and PKCE handling, duplicating a flow the platform already routes
  through the admin UI + device grant. The device flow is built and reused as-is.
- **Keep the matrix on the member surface** (fix the scorer instead of removing).
  Rejected: matrix is a batch, numeric-scored fan-out — not the chat-first member
  experience the owner chose — and its numeric scorer is a poor fit for
  text-producing model turns. It stays available via the CLI, where an explicit
  numeric scoring command is the right contract; the member surface drops it.
- **Delete `internal/matrix` entirely.** Rejected: the CLI (`internal/cli/matrix.go`)
  still uses it. Scope the removal to the member surface; revisit package deletion
  only if the CLI command is also retired.

## Out of scope for this ADR (explicit)

- **Any Go source, OpenAPI, or migration change.** This ADR writes only
  `docs/adr/`. The `cmd/member-app` binary, the matrix-view removal, and the
  "drun isn't running" panel are implemented by later batches, not here.
- **The concrete window/menu/icon ergonomics** and multi-window behavior (§7).
- **Auto-launching/bundling the drun engine** (§2) and **Windows/Linux packaging**
  (§5).
- **The loopback-OIDC-redirect auth variant** (§3) and **OS-keychain token
  storage** — reserved future enhancements, inherited from ADR-0001/0004.
- **`internal/matrix` package deletion and the CLI matrix command's future**
  (§4) — only the member-surface importer is dropped here.
- **Signing-key management, budget-cap evaluation, and proxy internals** — owned
  by ADR-0001, ADR-0003, and their batches; the app only consumes their outputs,
  exactly as ADR-0004 already established.

## References

- [ADR-0004](0004-member-conversational-console-architecture.md) — the member
  conversational console (the "member shell"): deployment shape (§1, revised
  here), device-flow auth (§2), launch + fail-closed verify (§3), the governed
  turn loop (§4), structure-only trajectory (§5), escalations-as-approvals (§6),
  and the deferred "native desktop app" alternative this ADR now adopts.
- [ADR-0001](0001-authentication-and-session-model.md) — the RFC 8628 device
  flow, JWT session token, distinct audiences, token storage, fail-closed.
- [ADR-0003](0003-session-launch-and-credential-grant.md) — session launch,
  signed policy bundle + fail-closed verify, scoped `proxy_token`, termination.
- `internal/memberui` — the server-rendered member surface reused verbatim in the
  window: `server.go` (`chi` router), `client.go`, `engine.go`, `run.go`,
  `checkpoints.go`, `commit.go`, `approvals.go`, `trajectory.go`, `matrix.go`
  (removed from the member surface), `README.md`.
- `cmd/member-console` — the plain, cgo-free web binary kept as the headless/dev
  entry point; `MEMBER_CONSOLE_HTTP_ADDR`, `DRUN_MCP_ENDPOINT`, and peers.
- `pkg/drunmcp` — the local sandbox client: `NewClient`, `Initialize` (liveness
  probe; no `/health`), `ListTools`, `CallTool`, `DefaultEndpoint`
  (`http://127.0.0.1:7273/mcp`), sequential-client constraint, `README.md`.
- `internal/matrix` — the numeric-scored parallel matrix engine (`parseScore`,
  `StatusScoreFailed`); kept for `internal/cli/matrix.go`, dropped from the member
  surface.
- `github.com/webview/webview_go` — the cgo native-webview library (system
  WebKit / WebView2 / WebKitGTK): `New`, `SetTitle`, `SetSize`, `Navigate`,
  `Run`, `Destroy`.
- `docs/TECH_STACK.md` — the locked Go + templ + HTMX, no-SPA/no-bundler stack the
  webview honors.
- `docs/security/threat-model.md`, design §3, §5.2, §6.3 — residency, the
  semi-trusted laptop, and the member-facing agent interface the window preserves.
