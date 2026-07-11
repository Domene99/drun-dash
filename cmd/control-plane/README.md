# control-plane

The drun-dash REST API and admin UI server. It exposes liveness/readiness
probes, the authentication vertical, and graceful shutdown.

## Endpoints

| Method | Path            | Auth        | Description                                              |
| ------ | --------------- | ----------- | -------------------------------------------------------- |
| GET    | `/healthz`      | none        | Liveness. Static `200` (no dependency checks).           |
| GET    | `/readyz`       | none        | Readiness. `200` when the database ping succeeds.        |
| POST   | `/auth/session` | OIDC token  | Exchange a verified OIDC ID token for a control-plane JWT. |
| GET    | `/auth/whoami`  | Bearer JWT  | Describe the authenticated principal.                    |

`/auth/session` and `/auth/whoami` are only mounted when OIDC is configured (see
below); otherwise the server logs a warning and serves the health probes only.

## Configuration

Base configuration (database, Redis, listen address, log level, environment) is
loaded by `internal/config` from the standard `DATABASE_URL`, `REDIS_URL`,
`HTTP_ADDR`, `LOG_LEVEL`, and `ENV` variables.

The authentication settings are read directly from the environment by the
control-plane wiring (`internal/config` is intentionally not extended for them):

| Variable               | Default         | Description                                                        |
| ---------------------- | --------------- | ------------------------------------------------------------------ |
| `CP_JWT_ISSUER`        | `drun-dash`     | `iss` claim stamped on minted access tokens.                       |
| `CP_JWT_AUDIENCE`      | `drun-dash-api` | `aud` claim; the control-plane API audience.                       |
| `CP_JWT_KID`           | `dev`           | Key id written to the JWT header and used to select a verify key.  |
| `CP_ACCESS_TOKEN_TTL`  | `15m`           | Access-token lifetime (any Go `time.ParseDuration` string).        |
| `CP_JWT_SIGNING_SEED`  | _(ephemeral)_   | Base64 of a 32-byte Ed25519 seed. Unset ⇒ a random per-process key. |
| `CP_OIDC_ISSUER`       | _(unset)_       | Org IdP issuer URL. Required to enable the auth routes.            |
| `CP_OIDC_CLIENT_ID`    | _(unset)_       | Org OAuth client id, checked as the ID token `aud`.               |

Notes:

- **`CP_JWT_SIGNING_SEED`** — when unset, an ephemeral signing key is generated
  at startup so a local `go run` works with no setup. Tokens signed with it do
  not survive a restart, so production **must** set a stable seed. Generate one
  with:

  ```sh
  openssl rand -base64 32
  ```

- **`CP_OIDC_ISSUER` / `CP_OIDC_CLIENT_ID`** — building the OIDC verifier runs
  live discovery against the issuer at startup. When either is unset the auth
  routes are not mounted (a warning is logged) and only the health probes are
  served. When set but unreachable/misconfigured, startup fails fast.
