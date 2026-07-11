# ADR-006: Conversational Dev Client Architecture

- **Status:** Accepted
- **Date:** 2026-07-11
- **Epic:** `epic/dev-client`
- **Tracker:** [Domene99/drun-dash#88](https://github.com/Domene99/drun-dash/issues/88)
- **Deciders:** Dev Client working group
- **Depends on:** ADR-001 (drun session model), ADR-003 (CLI / lifecycle contract)

> **Numbering note.** The tracker issue is labelled "ADR-005" in its title, but its
> acceptance criteria specify the file `docs/adr/006-conversational-client.md`. We honour
> the explicit filename and number this record **006**. If ADR-005 is later authored under
> a different topic, no renumbering is required; if not, this record may be aliased as 005.

---

## Context

`drun-dash` is the enterprise management surface for **drun** — a session-based sandboxed
compute platform. A drun *session* is a durable, forkable execution environment that exposes
a small set of primitives: run bash, read/write files, snapshot, checkpoint (label / squash /
drop), fork, merge, mount, and a fetch allowlist. Today those primitives are driven
programmatically (MCP tools) or via the `drun` CLI.

The **conversational dev client** is the developer-facing application in which a user *talks
to* a drun session in natural language, watches commands and file edits stream back, and
manages the session lifecycle (create → attach → checkpoint → fork → close) without hand-typing
CLI invocations. It is the counterpart to the management/admin web dashboard: the dashboard is
for operators and org-level control; the dev client is for an individual developer's hands-on
loop.

We must decide **the application architecture and packaging model** for this client, and
specifically **how it embeds the `drun` CLI and drives the drun session lifecycle**.

### Requirements driving the decision

| # | Requirement | Implication |
|---|-------------|-------------|
| R1 | Manage a long-lived local `drun` process / session and stream its stdout/stderr | Needs a real process supervisor, not just HTTP request/response |
| R2 | Bundle or discover the `drun` CLI binary and keep versions compatible | Needs OS-level packaging and a sidecar/exec story |
| R3 | Read/write local files the user drops into a session (mounts) | Needs native filesystem access outside the browser sandbox |
| R4 | Conversational, streaming UI (chat transcript, live command output, diffs) | Rich web-tech frontend (React/Svelte) is the pragmatic choice |
| R5 | Secure by default — sandboxed webview, no arbitrary local exec from page JS, honour drun's fetch allowlist posture | Strong renderer/backend isolation matters |
| R6 | Small install footprint & fast startup for daily developer use | Favours a lightweight shell |
| R7 | Cross-platform: macOS (primary), Linux, Windows | Rules out platform-locked toolkits |
| R8 | A path to a **hosted / web-only** variant later (attach to a remote drun daemon) | Frontend should be transport-agnostic and portable |

---

## Options considered

### Option A — Electron

Chromium + Node.js bundled per app.

- **Pros:** Mature ecosystem; Node's `child_process`/`node-pty` make spawning and PTY-driving
  the `drun` CLI trivial (R1/R2); huge library/support surface; team familiarity is common.
- **Cons:** Large binaries (~100–200 MB) and higher memory/startup cost (R6); a full Node
  runtime in the app increases the attack surface and demands disciplined `contextIsolation` /
  IPC hygiene (R5); auto-update and code-signing tooling is heavier.

### Option B — Tauri (v2)

Rust backend + the OS-native WebView (WKWebView / WebView2 / WebKitGTK), web frontend.

- **Pros:** Small binaries (~3–15 MB) and low memory/fast startup (R6); Rust core is a natural
  **process supervisor** for the drun lifecycle and gives a strong security model — capability-
  scoped IPC commands, no Node in the renderer, page JS cannot exec (R1/R5); first-class
  **sidecar** support to bundle the `drun` CLI binary and version-pin it (R2); native FS APIs
  for mounts (R3); the frontend is a plain web app, so a hosted/web-only build is a
  configuration change, not a rewrite (R8); cross-platform (R7).
- **Cons:** Backend logic is Rust (smaller talent pool than Node); relies on the host WebView,
  so rendering can vary slightly across platforms; ecosystem younger than Electron; some
  native integrations require writing Rust plugins.

### Option C — Web-only (browser SPA + local daemon)

Pure SPA served from the browser; a separately installed local `drun` daemon exposes an HTTP/WS
API the SPA talks to.

- **Pros:** Zero desktop packaging; instant updates; naturally supports remote/hosted drun (R8);
  simplest CI.
- **Cons:** A browser page **cannot** spawn or supervise a local CLI or touch the filesystem
  (R1/R2/R3) without a separately installed companion daemon — which just relocates the desktop
  problem and worsens onboarding; browser security (mixed content, CORS, localhost cert trust)
  fights the localhost-daemon pattern; no single "download and run" artifact for day-one users.

---

## Decision

**Adopt Tauri (v2) as the conversational dev client shell (Option B).**

1. **Shell:** Tauri v2. The Rust core owns process/lifecycle supervision and exposes a narrow,
   capability-scoped IPC command surface to the frontend. The renderer runs in the OS WebView
   with no Node and no direct exec capability.

2. **Frontend:** A transport-agnostic web app (TypeScript + React; framework choice deferred to
   a UI ADR). It speaks only to an internal client interface, never directly to a process. This
   keeps Option C reachable: swapping the IPC transport for an HTTP/WS transport yields a
   web-only build with no UI rewrite.

3. **Embedding the `drun` CLI (R2):** The `drun` binary ships as a **Tauri sidecar**, pinned to
   a known-compatible version and resolved per the ADR-003 CLI contract. A `drun` discovered on
   `PATH` may override the sidecar for power users, gated behind an explicit setting and a
   version-compatibility check.

4. **Session lifecycle (R1):** A Rust-side **Session Supervisor** is the single owner of drun
   session state within the app. It:
   - spawns the CLI (PTY where interactive I/O is needed),
   - maps drun lifecycle verbs — `create → attach → run → checkpoint (label/squash/drop) →
     snapshot → fork → merge → mount → close` — to typed IPC commands,
   - streams stdout/stderr/events to the frontend over a Tauri event channel,
   - persists session handles so the app can reattach after restart,
   - enforces graceful teardown (checkpoint-or-prompt before close) so no work is silently lost.

5. **Security posture (R5):** Default-deny IPC capabilities; the renderer gets only the commands
   it needs. The client surfaces and respects drun's **fetch allowlist** rather than widening
   it. CLI invocation is parameterised (no shell string interpolation of user input). Sidecar
   integrity is verified at package time via signing/hashing.

6. **Web-only variant (Option C):** **Deferred, not discarded.** Recorded as the intended second
   target once a remote drun daemon transport exists. The web-app + transport-abstraction design
   above is the explicit enabler; we do not build the daemon now.

7. **Electron (Option A):** **Rejected** for footprint, startup cost, and the larger security
   surface of an embedded Node runtime — none of which buy us anything the Rust core doesn't
   already provide for the lifecycle/supervision work that dominates this client.

### Architecture at a glance

```
┌──────────────────────────────────────────────────────────┐
│                  Tauri App (desktop)                       │
│                                                            │
│  ┌────────────────────┐        ┌───────────────────────┐  │
│  │  WebView (renderer)│  IPC   │  Rust Core            │  │
│  │  React chat UI     │◄──────►│  ┌─────────────────┐  │  │
│  │  - transcript      │ events │  │ Session         │  │  │
│  │  - live output     │        │  │ Supervisor      │  │  │
│  │  - diff / files    │        │  └───────┬─────────┘  │  │
│  │  (no exec, no Node)│        │          │ spawn/PTY  │  │
│  └────────────────────┘        │  ┌───────▼─────────┐  │  │
│         ▲ transport-agnostic   │  │ drun CLI        │  │  │
│         │ client interface     │  │ (sidecar,       │  │  │
│         │ (IPC now, HTTP/WS    │  │  version-pinned)│  │  │
│         │  later → web-only)   │  └───────┬─────────┘  │  │
│  └──────┘                      └──────────┼────────────┘  │
└───────────────────────────────────────────┼──────────────┘
                                             ▼
                                    drun session platform
```

---

## MVP feature list (scoped)

The MVP is the smallest client that lets one developer hold a productive conversation with a
single drun session end-to-end.

**In scope (MVP)**

1. **App shell & packaging** — Tauri v2 app; signed builds for macOS (primary) + Linux;
   Windows best-effort. Auto-update deferred (manual download acceptable for MVP).
2. **CLI embedding** — bundle `drun` as a version-pinned sidecar; startup health/version check
   with a clear error if incompatible.
3. **Session lifecycle (happy path)** — create a session, attach, and close from the UI;
   reattach to the most recent session on relaunch.
4. **Conversational loop** — chat transcript UI; send a natural-language turn; stream the
   resulting command execution and stdout/stderr live into the transcript.
5. **Command & file visibility** — render executed bash commands, their output, and file
   read/write results (basic diff view) inline in the conversation.
6. **Checkpoints (manual)** — create and label a checkpoint from the UI; list checkpoints for
   the current session.
7. **Graceful teardown** — prompt to checkpoint on close; never drop an active session silently.
8. **Security defaults** — capability-scoped IPC; parameterised CLI calls; display (not widen)
   the session's fetch allowlist.
9. **Errors & logs** — surface CLI/lifecycle errors legibly; local diagnostic log for support.

**Explicitly deferred (post-MVP)**

- Web-only / hosted-daemon variant (transport swap).
- Fork / merge / snapshot-tree visualisation and multi-session management.
- Checkpoint squash/drop and history rewrite from the UI.
- Mounts UI (drag-drop local folders into a session).
- Auto-update, telemetry, and org/SSO integration (belongs with the management dashboard).
- Multi-pane / multi-session workspace and collaboration.
- Rich agentic tooling beyond the single conversational loop.

---

## Consequences

**Positive**

- Small, fast, secure client suited to daily developer use (R5/R6).
- The Rust core is a purpose-built home for lifecycle supervision, PTY handling, and sidecar
  management — the parts that are hardest and most safety-critical (R1/R2).
- Transport-agnostic frontend keeps the web-only future cheap (R8) without building it now.
- Sidecar packaging gives deterministic CLI/version pairing and clean upgrades (R2).

**Negative / risks**

- Rust skill requirement for backend work; mitigate with a thin, well-documented supervisor
  crate and keeping business logic in the (TypeScript) frontend where possible.
- WebView rendering differences across OSes; mitigate with a supported-WebView matrix and CI
  smoke tests per platform.
- Younger ecosystem than Electron; mitigate by preferring stable Tauri v2 plugins and isolating
  any native gaps behind small Rust plugins.
- Sidecar bloat if the `drun` binary is large; mitigate with per-target builds and lazy fetch of
  optional components if needed.

**Follow-ups**

- UI framework & design-system ADR for the frontend.
- Formalise the CLI/lifecycle contract consumed here against ADR-003 (command names, exit codes,
  event stream format, version-compatibility policy).
- Auto-update & code-signing ADR before GA.
- Web-only transport ADR when the remote drun daemon is scheduled.

---

## References

- Tracker issue #88 — [Dev Client] Conversational app architecture
- ADR-001 — drun session model *(dependency)*
- ADR-003 — CLI / lifecycle contract *(dependency)*
- Tauri v2 — sidecar & capability model; Electron — process/security model (option research)
