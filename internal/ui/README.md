# internal/ui

Server-rendered admin web app for drun-dash — **Go + [templ](https://templ.guide) + [HTMX](https://htmx.org)**.

This package is the guts of the `admin-ui` binary (`cmd/admin-ui`). It renders a
base layout, a login page, and a dashboard that reads **live data** from the
control-plane REST API. It is a deliberately bounded shell: it establishes the
binary, the templ+HTMX toolchain, and the API-client pattern. Broader
functionality lands in later batches.

## Design

The admin UI is a **separate binary** from the control plane and **does not
import control-plane packages**. It is fully decoupled: it talks to the control
plane over HTTP through `Client` (`client.go`), hitting the existing REST
endpoints (`/auth/whoami`, `/agents`, `/sessions`, `/analytics/spend`).

```
cmd/admin-ui/main.go
  └─ ui.New(client, logger)          server.go   chi router + auth middleware
       ├─ views.Layout/Login/Dashboard/Summary   (templ)   presentation only
       └─ ui.Client ── HTTP ──▶ control-plane REST API
```

- **`client.go`** — a tiny read-only control-plane client. Each call sends the
  operator's session token as `Authorization: Bearer …`, decodes the typed
  success body, and is aware of the flat `Error` envelope (`{code, message}`,
  per ADR-0002). A `401` maps to the `ErrUnauthorized` sentinel; other non-2xx
  responses map to `*APIError`.
- **`server.go`** — a `chi` router and the request handlers. Routes:
  - `GET /healthz` — static liveness probe.
  - `GET /login` — render the sign-in form.
  - `POST /login` — store the pasted token in the session cookie, redirect to `/`.
  - `POST /logout` — clear the cookie, redirect to `/login`.
  - `GET /` — the dashboard (behind auth middleware).
  - `GET /fragments/summary` — the HTMX-refreshed live-overview fragment.
  The auth middleware redirects to `/login` whenever the session cookie is
  absent, and clears a stale cookie when the control plane rejects the token.
- **`views/`** — the templ components (`*.templ`) and their generated
  (`*_templ.go`) Go. Views are pure presentation and receive fully-resolved
  view models built by `server.go`.

HTMX is loaded from a CDN `<script>` in the layout and drives the dashboard's
summary fragment, which polls `GET /fragments/summary` every 30s and also has a
manual **Refresh** button.

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
against a mock control-plane server; the server is driven with recorded
requests to assert the redirect-when-unauthenticated, cookie-on-login, and
dashboard-renders-from-live-data behaviors.
