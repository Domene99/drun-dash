# internal/ui

Server-rendered admin web app for drun-dash — **Go + [templ](https://templ.guide) + [HTMX](https://htmx.org)**.

This package is the guts of the `admin-ui` binary (`cmd/admin-ui`). It renders a
base layout, a login page, a dashboard, and the **governance pages** — Sessions,
Spend, and Approvals — all reading **live data** from the control-plane REST
API. It is fully decoupled from the control plane: it talks to it only over
HTTP.

## Design

The admin UI is a **separate binary** from the control plane and **does not
import control-plane packages**. It talks to the control plane over HTTP through
`Client` (`client.go`), hitting the existing REST endpoints (`/auth/whoami`,
`/agents`, `/sessions`, `/analytics/spend`, `/analytics/budgets`, `/approvals`,
`/approvals/{id}/decision`).

```
cmd/admin-ui/main.go
  └─ ui.New(client, logger)          server.go   chi router + auth middleware
       ├─ views.Layout/Login/Dashboard/Summary   (templ)   presentation only
       ├─ views.Sessions/Spend/Approvals          (templ)   governance pages
       └─ ui.Client ── HTTP ──▶ control-plane REST API
```

- **`client.go`** — a tiny control-plane client. Each call sends the operator's
  session token as `Authorization: Bearer …`, decodes the typed success body,
  and is aware of the flat `Error` envelope (`{code, message}`, per ADR-0002). A
  `401` maps to the `ErrUnauthorized` sentinel; other non-2xx responses map to
  `*APIError`. Read helpers: `Whoami`, `ListAgents`, `ListSessions`,
  `QuerySpend`, `ListBudgets`, `ListApprovals`, `GetSessionTrajectory`
  (`GET /sessions/{sessionId}/trajectory`). Write helper: `DecideApproval`
  (`POST /approvals/{id}/decision`).
- **`server.go`** — a `chi` router and the request handlers. Routes:
  - `GET /healthz` — static liveness probe.
  - `GET /login` — render the sign-in form.
  - `POST /login` — store the pasted token in the session cookie, redirect to `/`.
  - `POST /logout` — clear the cookie, redirect to `/login`.
  - `GET /` — the dashboard (behind auth middleware).
  - `GET /fragments/summary` — the HTMX-refreshed live-overview fragment.
  - `GET /sessions` — the Sessions page (flight recorder).
  - `GET /sessions/{sessionId}/trajectory` — the forensic trajectory replay
    viewer (the session's captured decision graph).
  - `GET /spend` — the Spend page (finance view): total, per-group breakdown, and
    budget utilization.
  - `GET /approvals` — the Approvals inbox.
  - `POST /approvals/{id}/decision` — the HTMX approve/deny action; the handler
    calls `DecideApproval` and re-renders just the affected row.
  The auth middleware redirects to `/login` whenever the session cookie is
  absent, and clears a stale cookie when the control plane rejects the token.
- **`views/`** — the templ components (`*.templ`) and their generated
  (`*_templ.go`) Go. Views are pure presentation and receive fully-resolved
  view models built by `server.go`.

### Governance pages

- **Sessions** (`views/sessions.templ`) — a table of governed sessions: id,
  agent, status, created, and fan-out ceiling. Full-page GET.
- **Spend** (`views/spend.templ`) — the finance view: total spend over the fixed
  window plus a per-agent breakdown, and a budgets table showing spent vs limit
  and utilization (a budget at/over its limit is flagged). The spend and budgets
  sections fail independently, so one degraded endpoint does not hide the other.
- **Trajectory** (`views/trajectory.templ`) — the **forensic trajectory replay
  viewer**, the design's signature audit artifact. Given a session it fetches
  `GET /sessions/{sessionId}/trajectory` and reconstructs the flat
  `TrajectoryEvent` list into a tree (roots are events with no/absent
  `parent_event_id`; children are grouped by parent). The tree is flattened
  **depth-first in occurrence order** in Go (`trajectoryRows`) and rendered as an
  indented table: each row shows the event type, a field/tool summary (payload
  fields show their captured value; structure fields show type and byte size),
  the `field_class` **structure**/**payload** badge, and the occurrence time. A
  legend explains the badges. The Sessions table links each row to its
  trajectory view. A `404` (unknown session) is surfaced inline, not as a blank
  page. Server-rendered; no JS graph library (a richer canvas is a follow-up).
- **Approvals** (`views/approvals.templ`) — the escalation queue. Pending items
  render **Approve** / **Deny** buttons that HTMX-`POST` to
  `/approvals/{id}/decision` and swap the single `<tr>` (outerHTML) with the
  re-rendered result; already-decided items are read-only. A failed decision
  (for example a `409` on an already-decided request) is surfaced inside the row
  rather than blanking the page.

Server-rendered throughout: full-page GETs for the tables, HTMX only for the
summary refresh and the approve/deny row-swap. No client-side state framework.

**Error handling.** Every page fetches server-side. A rejected token
(`ErrUnauthorized`) clears the cookie and redirects to `/login` (via
`HX-Redirect` for HTMX requests); any other control-plane error is rendered as a
readable inline message, never a blank page.

## Auth (v1, bounded)

There is **no OIDC browser flow yet**. For this shell the operator obtains a
control-plane session token out-of-band — e.g. `drun-dash login` (the CLI) or a
direct `POST /auth/session` — and pastes it into the login page. The server
stores it in a **secure, HttpOnly, SameSite=Lax** cookie (`dd_session`) and
forwards it as a Bearer credential on every API call. `Secure` is enabled in
production (`ENV=production`) and off for plain-HTTP local dev.

> **Follow-up:** the full OIDC **authorization-code** browser flow (redirect to
> the IdP, handle the callback, exchange the code) is a documented next step. It
> would replace the paste-a-token login while keeping the cookie/session model.

## Configuration

The `admin-ui` binary reads two settings from the environment (shared settings
come from `internal/config`):

| Variable | Default | Purpose |
| --- | --- | --- |
| `ADMIN_UI_HTTP_ADDR` | `:8082` | admin UI listen address |
| `CONTROL_PLANE_BASE_URL` | `http://localhost:8080` | control-plane base URL |

## Working with templ

The generated `*_templ.go` files are **committed**, so `go build`, `go test`,
and CI need no templ binary. Regenerate them whenever you edit a `.templ` file:

```sh
go run github.com/a-h/templ/cmd/templ generate ./internal/ui/...
# or, with the binary installed:
templ generate
```

Never hand-edit `*_templ.go` — change the `.templ` source and regenerate.

## Tests

`client_test.go` and `server_test.go` use `httptest`: the client is exercised
against a mock control-plane server; the server is driven with recorded requests
to assert the redirect-when-unauthenticated, cookie-on-login, and
render-from-live-data behaviors for every page, plus the approve/deny action
(POST → `DecideApproval` → row re-render) and readable error surfacing.
