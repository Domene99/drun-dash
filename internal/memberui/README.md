# internal/memberui

The **member console**: a small, server-rendered web app (Go + templ + HTMX) an
organization member runs on their own workstation. It is the shell for the
second frontend (design §6.3); the conversational run view arrives in a later
batch (ADR-0004).

It is a separate binary (`cmd/member-console`) from the control plane and never
imports control-plane packages — it talks to the control plane over HTTP through
[`Client`](client.go), sending the member's session token as a Bearer
credential. It deliberately mirrors `internal/ui` (the admin UI) without
importing it: the two frontends serve different audiences, so the small
server/cookie/client pieces are replicated rather than shared.

## What it does

- **Sign in (device flow, RFC 8628).** `GET /login` calls
  `POST /auth/device/code`, shows the `user_code` and a verification link, and
  stashes the `device_code` in a transient HttpOnly cookie. An HTMX poller hits
  `GET /login/poll`, which calls `POST /auth/device/token`; on approval the
  minted control-plane session JWT is stored in a secure, HttpOnly cookie and
  HTMX navigates to the home page. Pending/slow-down keep polling; denied/expired
  stop with a message.
- **Entitled agents.** `GET /` lists the agents the member may launch. The
  client prefers `GET /me/agents` and falls back to `GET /agents` when the former
  is not present (feature-detected on a 404), so it does not hard-depend on the
  merge order of the `/me/agents` endpoint.
- **Launch / terminate.** `POST /sessions` launches the chosen agent
  (`POST /sessions/launch`) and `POST /sessions/{id}/terminate` ends it. The
  launch response's `proxy_token` is a secret: it is held **server-side** in the
  console's in-memory session store and is never rendered to the page or written
  to logs. Only the session id, proxy base URL, fan-out ceiling, and expiry are
  shown.
- **Health.** `GET /healthz` returns a static `{"status":"ok"}` without
  consulting the control plane.

## Configuration

Read by `cmd/member-console` from the environment (see that command's doc
comment):

| Variable                    | Default                        | Purpose                          |
| --------------------------- | ------------------------------ | -------------------------------- |
| `MEMBER_CONSOLE_HTTP_ADDR`  | `:8083`                        | Console listen address           |
| `CONTROL_PLANE_BASE_URL`    | `http://localhost:8080`        | Control-plane base URL           |
| `MEMBER_CONSOLE_CLIENT_ID`  | `drun-dash-member-console`     | Device-flow `client_id`          |

Secure cookies are enabled only in production (HTTPS); plain-HTTP localhost
development leaves them off.

## Views

The `*.templ` files under `views/` are the source of truth; the committed
`*_templ.go` files are generated with `templ generate` (pinned to the
`github.com/a-h/templ` version in `go.mod`) so CI needs no templ binary. Edit the
`.templ` file and regenerate — never hand-edit the generated Go.
