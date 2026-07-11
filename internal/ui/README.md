# internal/ui

Server-rendered admin web app for drun-dash — **Go + [templ](https://templ.guide) + [HTMX](https://htmx.org)**.

This package is the guts of the `admin-ui` binary (`cmd/admin-ui`). It renders a
base layout, a login page, a dashboard, the **governance pages** — Sessions,
Spend, and Approvals — and the **device-approval page** the CLI device flow
needs, all reading **live data** from the control-plane REST API. It is fully
decoupled from the control plane: it talks to it only over HTTP.

## Design

The admin UI is a **separate binary** from the control plane and **does not
import control-plane packages**. It talks to the control plane over HTTP through
`Client` (`client.go`), hitting the existing REST endpoints (`/auth/whoami`,
`/auth/session`, `/auth/device/approve`, `/agents`, `/sessions`,
`/analytics/spend`, `/analytics/budgets`, `/approvals`,
`/approvals/{id}/decision`).

```
cmd/admin-ui/main.go
  └─ ui.New(client, logger, ui.WithOIDC(authr))   server.go   chi router + auth middleware
       ├─ ui.OIDCAuthenticator                     oidc.go     authorization-code + PKCE
       ├─ views.Layout/Login/Dashboard/Summary     (templ)     presentation only
       ├─ views.Sessions/Spend/Approvals/Device    (templ)     governance + device pages
       └─ ui.Client ── HTTP ──▶ control-plane REST API
```

- **`oidc.go`** — the OIDC authorization-code flow. `OIDCConfigFromEnv` reads
  the settings; `NewOIDCAuthenticator` performs OIDC discovery against the
  issuer to learn the authorize/token endpoints and builds an
  `golang.org/x/oauth2` config. `StartLogin` mints a CSRF state token and a PKCE
  verifier and returns the authorize URL (carrying the state and an S256 code
  challenge); `Exchange` trades the returned code (with the verifier) for the
  token set and returns the raw **ID token**. The admin UI does **not** verify
  the ID token — the control plane does, on `/auth/session`.
- **`client.go`** — a tiny control-plane client. Each authenticated call sends
  the operator's session token as `Authorization: Bearer …`, decodes the typed
  success body, and is aware of the flat `Error` envelope (`{code, message}`,
  per ADR-0002). A `401` maps to the `ErrUnauthorized` sentinel; other non-2xx
  responses map to `*APIError`. Read helpers: `Whoami`, `ListAgents`,
  `ListSessions`, `QuerySpend`, `ListBudgets`, `ListApprovals`,
  `GetSessionTrajectory`. Write helpers: `DecideApproval`
  (`POST /approvals/{id}/decision`), `ExchangeSession` (`POST /auth/session`,
  unauthenticated — trades the OIDC ID token for a session JWT), and
  `ApproveDevice` (`POST /auth/device/approve` — approves/denies a CLI device
  request with the member's Bearer token).
- **`server.go`** — a `chi` router and the request handlers. Routes:
  - `GET /healthz` — static liveness probe.
  - `GET /login` — with OIDC configured, redirect to the IdP authorize URL
    (state + PKCE stored in transient cookies); otherwise render the
    paste-a-token dev form.
  - `POST /login` — the dev fallback: store the pasted token in the session
    cookie, redirect to `/`.
  - `GET /callback` — the OIDC redirect target: validate state, exchange the
    code, POST the ID token to `/auth/session`, store the returned session JWT
    in the cookie, land on the dashboard.
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
  - `GET /device?user_code=…` — the CLI device-approval confirmation page
    (behind auth).
  - `POST /device` — record the approve/deny decision via `ApproveDevice`.
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
  `TrajectoryEvent` list into a tree, flattened **depth-first in occurrence
  order** and rendered as an indented table. A `404` (unknown session) is
  surfaced inline, not as a blank page.
- **Approvals** (`views/approvals.templ`) — the escalation queue. Pending items
  render **Approve** / **Deny** buttons that HTMX-`POST` to
  `/approvals/{id}/decision` and swap the single `<tr>`; already-decided items
  are read-only.
- **Device** (`views/device.templ`) — the CLI **device-approval** page. It shows
  the `user_code` from the query so the signed-in member can confirm it matches
  their terminal, then approves or denies via `POST /device` →
  `ApproveDevice`. This is the browser step that completes `drun-dash login`.

Server-rendered throughout: full-page GETs for the tables, HTMX only for the
summary refresh and the approve/deny row-swap. No client-side state framework.

**Error handling.** Every page fetches server-side. A rejected token
(`ErrUnauthorized`) clears the cookie and redirects to `/login` (via
`HX-Redirect` for HTMX requests); any other control-plane error is rendered as a
readable inline message, never a blank page.

## Auth

### OIDC authorization-code flow (default)

When OIDC is configured, sign-in is the standard browser flow:

1. `GET /login` mints a CSRF **state** token and a **PKCE** verifier (storing
   both in short-lived, HttpOnly cookies) and redirects to the IdP's authorize
   URL with an S256 code challenge.
2. The IdP authenticates the member and redirects back to
   `GET /callback?code=…&state=…`.
3. The callback validates the `state` against the cookie (constant-time),
   exchanges the `code` (with the PKCE verifier) for the token set, and takes
   the **ID token**.
4. It POSTs the ID token to the control plane's `POST /auth/session`
   (`{id_token, organization_id}`); the control plane **verifies** the ID token
   and returns the session JWT (`TokenResponse`).
5. The JWT is stored in the **secure, HttpOnly, SameSite=Lax** `dd_session`
   cookie and the member lands on the dashboard.

The admin UI never verifies the ID token itself — verification (signature +
`iss`/`aud`/`exp`) is the control plane's job (`internal/oidc` +
`internal/auth`). `Secure` is enabled in production (`ENV=production`) and off
for plain-HTTP local dev.

### Paste-a-token dev fallback

If the OIDC environment is **unset** (see Configuration), the browser flow is
disabled and `GET /login` renders the paste-a-token form instead: the operator
obtains a control-plane session token out-of-band — e.g. `drun-dash login` or a
direct `POST /auth/session` — and pastes it. This keeps local development
working without an identity provider. The cookie/session model is identical.

## Configuration

The `admin-ui` binary reads these settings from the environment (shared settings
come from `internal/config`):

| Variable | Default | Purpose |
| --- | --- | --- |
| `ADMIN_UI_HTTP_ADDR` | `:8082` | admin UI listen address |
| `CONTROL_PLANE_BASE_URL` | `http://localhost:8080` | control-plane base URL |
| `OIDC_ISSUER` | _(unset)_ | identity-provider issuer URL |
| `OIDC_CLIENT_ID` | _(unset)_ | registered OAuth client ID |
| `OIDC_CLIENT_SECRET` | _(unset)_ | OAuth client secret (confidential clients) |
| `ADMIN_UI_REDIRECT_URL` | _(unset)_ | this app's `/callback` URL registered with the IdP |
| `DEFAULT_ORGANIZATION_ID` | _(unset)_ | org forwarded to the control plane's `/auth/session` |

The OIDC browser flow is enabled only when `OIDC_ISSUER`, `OIDC_CLIENT_ID`, and
`ADMIN_UI_REDIRECT_URL` are all set; otherwise the paste-a-token dev login stays
active.

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

`client_test.go`, `server_test.go`, and `oidc_test.go` use `httptest`: the
client is exercised against a mock control-plane server (including
`ExchangeSession` and `ApproveDevice`); the server is driven with recorded
requests against a **mock IdP** and a **mock control plane** to assert the OIDC
`/login` → `/callback` exchange (state/PKCE validated, session cookie set), the
device approve/deny flow, the dev paste-token fallback when OIDC is unset, and
the redirect-when-unauthenticated / readable-error behaviors for every page.
