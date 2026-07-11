# drun-dash Runbook — full-stack local deployment

How to build and run the whole drun-dash platform (Postgres + Redis + the three
service containers) with one command, plus the port map, per-service env
reference, startup order, health endpoints, and how to seed demo data.

For the infrastructure-only dev stack (just Postgres + Redis, no app
containers) see [`deploy/docker/README.md`](../deploy/docker/README.md). This
runbook covers the **application** stack in
[`deploy/docker/docker-compose.app.yml`](../deploy/docker/docker-compose.app.yml).

> **Scope.** This is a container-based local/single-host deployment.
> Kubernetes/Helm manifests are intentionally **out of v1 scope** (the target
> is a hosted control plane) and are a possible future addition.

---

## 1. Prerequisites

- Docker Engine with the Compose v2 plugin (`docker compose`, not the legacy
  `docker-compose`).
- No local Go toolchain needed — each service is built inside a multi-stage
  image on `golang:1.22`.

---

## 2. Architecture at a glance

Three server binaries plus supporting infrastructure, all on one compose
network:

| Component       | Image base                     | Port  | Depends on                          |
| --------------- | ------------------------------ | ----- | ----------------------------------- |
| `postgres`      | `postgres:16`                  | 5432  | —                                   |
| `redis`         | `redis:7`                      | 6379  | —                                   |
| `migrate`       | `migrate/migrate:v4.18.1`      | —     | postgres (healthy); runs once, exits|
| `control-plane` | `golang:1.22` → `alpine:3.20`  | 8080  | postgres, redis, migrate            |
| `llm-proxy`     | `golang:1.22` → `alpine:3.20`  | 8081  | postgres, redis, migrate            |
| `admin-ui`      | `golang:1.22` → `alpine:3.20`  | 8082  | control-plane (healthy)             |

Each service image is a multi-stage build: a static, CGO-free Linux binary
compiled on `golang:1.22`, copied onto a minimal Alpine runtime, and run as a
**non-root** user (uid 65532).

---

## 3. Ports

| Service         | In-container | Host port (env var)          | URL                     |
| --------------- | ------------ | ---------------------------- | ----------------------- |
| control-plane   | 8080         | `HTTP_PORT` (8080)           | http://localhost:8080   |
| llm-proxy       | 8081         | `PROXY_HTTP_PORT` (8081)     | http://localhost:8081   |
| admin-ui        | 8082         | `ADMIN_UI_HTTP_PORT` (8082)  | http://localhost:8082   |
| postgres        | 5432         | `POSTGRES_PORT` (5432)       | postgres://localhost:5432 |
| redis           | 6379         | `REDIS_PORT` (6379)          | redis://localhost:6379  |

---

## 4. Build & run

From `deploy/docker/`:

```bash
# 1. Create your env file (defaults are fine for a first local run).
cp .env.app.example .env.app

# 2. Build the images and bring the whole stack up in the background.
docker compose --env-file .env.app -f docker-compose.app.yml up --build -d

# 3. Watch startup / health.
docker compose --env-file .env.app -f docker-compose.app.yml ps

# 4. Tail logs (all services, or one).
docker compose --env-file .env.app -f docker-compose.app.yml logs -f
docker compose --env-file .env.app -f docker-compose.app.yml logs -f control-plane

# 5. Stop the stack (keeps data volumes).
docker compose --env-file .env.app -f docker-compose.app.yml down

# 6. Stop and wipe data volumes (fresh database next time).
docker compose --env-file .env.app -f docker-compose.app.yml down -v
```

Validate the compose file without running anything:

```bash
docker compose -f docker-compose.app.yml config
```

This parses even with **no** env file, because every value has an inline
`${VAR:-default}` dev default.

### Build a single image directly

Build from the **repository root** (the build context includes `go.mod` and
`./cmd`):

```bash
docker build -f deploy/docker/Dockerfile.control-plane -t drundash-control-plane .
docker build -f deploy/docker/Dockerfile.llm-proxy     -t drundash-llm-proxy .
docker build -f deploy/docker/Dockerfile.admin-ui      -t drundash-admin-ui .
```

An optional `--build-arg VERSION=v1.2.3` stamps `main.version` (surfaced by
each binary's `--version` flag).

---

## 5. Startup order

Ordering is enforced with healthchecks and `depends_on` conditions, so nothing
starts against an unready dependency:

```
postgres (healthy) ─┐
redis    (healthy) ─┤
                    ├─> migrate (runs migrations once, exits 0)
                    │        │
                    │        ├─> control-plane (readyz healthy) ─> admin-ui
                    │        └─> llm-proxy
```

- **postgres / redis** report `healthy` via `pg_isready` / `redis-cli ping`.
- **migrate** waits for `postgres: service_healthy`, applies all migrations in
  [`migrations/`](../migrations) (golang-migrate `up`), then exits `0`. The app
  services wait on `migrate: service_completed_successfully`, so they never see
  a half-migrated schema.
- **control-plane** becomes `healthy` only when `/readyz` returns `200` (DB
  ping succeeds). **admin-ui** waits for that before starting.

Re-running `up` after the first boot re-runs `migrate`; migrations are
idempotent (already-applied versions are skipped), so this is a no-op on an
up-to-date schema.

---

## 6. Health endpoints

| Service         | Liveness            | Readiness                              |
| --------------- | ------------------- | -------------------------------------- |
| control-plane   | `GET /healthz` 200  | `GET /readyz` 200 when DB ping succeeds|
| llm-proxy       | `GET /healthz` 200  | — (uses `/healthz`)                     |
| admin-ui        | `GET /healthz` 200  | — (uses `/healthz`)                     |

Quick manual check once the stack is up:

```bash
curl -fsS http://localhost:8080/healthz && echo cp-live
curl -fsS http://localhost:8080/readyz  && echo cp-ready
curl -fsS http://localhost:8081/healthz && echo proxy-live
curl -fsS http://localhost:8082/healthz && echo ui-live
```

The compose healthchecks use busybox `wget` against these same paths from
inside each container.

---

## 7. Seeding demo data

[`deploy/seed/seed.sql`](../deploy/seed/seed.sql) loads a repeatable,
**dev-only** demo dataset (one `demo` org with environments, teams, members,
endpoints, a policy, a budget, and two agents). It requires the schema to be
migrated first — with the stack up, that is already done.

Apply it against the running Postgres container from the repository root:

```bash
docker compose --env-file deploy/docker/.env.app \
  -f deploy/docker/docker-compose.app.yml \
  exec -T postgres \
  psql -U drundash -d drundash < deploy/seed/seed.sql
```

Or with a local `psql` client over the mapped host port:

```bash
export DATABASE_URL='postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable'
psql "$DATABASE_URL" -f deploy/seed/seed.sql
```

The script is idempotent — safe to re-run. See
[`deploy/seed/README.md`](../deploy/seed/README.md) for details.

---

## 8. Environment reference

All values are set in `deploy/docker/.env.app` (copied from `.env.app.example`).
Each also has an inline dev default in the compose file. **Secrets are
placeholders — never commit real keys.** `.env.app` is git-ignored.

### Shared / infrastructure

| Variable            | Default (dev)                                                        | Notes                                            |
| ------------------- | -------------------------------------------------------------------- | ------------------------------------------------ |
| `POSTGRES_USER`     | `drundash`                                                           | Postgres role                                    |
| `POSTGRES_PASSWORD` | `drundash`                                                           | Dev only — change for anything shared            |
| `POSTGRES_DB`       | `drundash`                                                           | Database name                                    |
| `DATABASE_URL`      | `postgres://drundash:drundash@postgres:5432/drundash?sslmode=disable`| Service DNS host `postgres`, not `localhost`     |
| `REDIS_URL`         | `redis://redis:6379/0`                                               | Service DNS host `redis`                         |
| `LOG_LEVEL`         | `info`                                                               | `debug` \| `info` \| `warn` \| `error`           |
| `ENV`               | `development`                                                        | `development` \| `staging` \| `production`       |

### control-plane (`:8080`)

| Variable                 | Default (dev)             | Notes                                                              |
| ------------------------ | ------------------------- | ----------------------------------------------------------------- |
| `HTTP_ADDR`              | `:8080`                   | In-container listen address                                       |
| `CP_JWT_ISSUER`          | `drun-dash`               | `iss` claim on minted access tokens                               |
| `CP_JWT_AUDIENCE`        | `drun-dash-api`           | `aud` claim (the API audience)                                    |
| `CP_JWT_KID`             | `dev`                     | Key id in the JWT header                                          |
| `CP_ACCESS_TOKEN_TTL`    | `15m`                     | Access-token lifetime (`time.ParseDuration`)                      |
| `CP_JWT_SIGNING_SEED`    | _(unset → ephemeral)_     | Base64 32-byte Ed25519 seed. **Prod must set** (`openssl rand -base64 32`) |
| `CP_OIDC_ISSUER`         | _(unset)_                 | IdP issuer URL. Set with client id to enable auth routes          |
| `CP_OIDC_CLIENT_ID`      | _(unset)_                 | OAuth client id (checked as ID-token `aud`)                       |
| `CP_BUNDLE_SIGNING_SEED` | _(unset → ephemeral)_     | Base64 32-byte Ed25519 seed for signing policy bundles            |
| `CP_BUNDLE_SIGNING_KID`  | `dev`                     | Bundle signing key id                                             |
| `CP_PROXY_BASE_URL`      | `http://llm-proxy:8081`   | Proxy base URL handed to launched sessions                        |
| `CP_SESSION_TTL`         | `60m`                     | Session (bundle + grant) lifetime                                 |

When `CP_OIDC_ISSUER` / `CP_OIDC_CLIENT_ID` are unset, the control plane serves
only the health probes (auth routes are not mounted). When set but
unreachable, startup fails fast.

### llm-proxy (`:8081`)

| Variable                     | Default (dev)              | Notes                                                          |
| ---------------------------- | -------------------------- | ------------------------------------------------------------- |
| `PROXY_HTTP_ADDR`            | `:8081`                    | In-container listen address                                   |
| `LLM_UPSTREAM_BASE_URL`      | `https://api.openai.com`   | Upstream provider base URL (scheme + host)                    |
| `DRUNDASH_SECRET_ANTHROPIC`  | _(unset — **placeholder**)_| Master upstream key. Dev EnvVault maps `vault://dev/anthropic` → this var. Unset ⇒ broker returns `502` |

The upstream *destination* is the single configured `LLM_UPSTREAM_BASE_URL`; the
upstream *credential* is resolved per grant from the vault (dev backend reads
`DRUNDASH_SECRET_*`). Add more `DRUNDASH_SECRET_<NAME>` vars for other endpoints.

### admin-ui (`:8082`)

| Variable                 | Default (dev)                | Notes                                              |
| ------------------------ | ---------------------------- | -------------------------------------------------- |
| `ADMIN_UI_HTTP_ADDR`     | `:8082`                      | In-container listen address                        |
| `CONTROL_PLANE_BASE_URL` | `http://control-plane:8080`  | Control-plane API base URL (service DNS name)      |

The admin UI never dials Postgres/Redis, but `config.Load` still validates
`DATABASE_URL`/`REDIS_URL` at boot — the dev defaults are valid URLs, so it
passes. Operators sign in by pasting a control-plane session token.

---

## 9. Troubleshooting

- **`control-plane` never becomes healthy** — check `/readyz`; it stays `503`
  until Postgres is reachable and migrated. Inspect `logs migrate` and
  `logs control-plane`.
- **`migrate` exits non-zero** — a dirty/failed migration. Inspect
  `logs migrate`; for a clean slate run `down -v` and `up` again.
- **`llm-proxy` returns `502 upstream_unconfigured`** — `LLM_UPSTREAM_BASE_URL`
  is missing/invalid. `502 upstream_credential_unavailable` — the endpoint's
  secret (e.g. `DRUNDASH_SECRET_ANTHROPIC`) is unset.
- **Port already in use** — change the host-side port var (`HTTP_PORT`,
  `PROXY_HTTP_PORT`, `ADMIN_UI_HTTP_PORT`, `POSTGRES_PORT`, `REDIS_PORT`).
- **Secrets don't survive restart** — set stable `CP_JWT_SIGNING_SEED` and
  `CP_BUNDLE_SIGNING_SEED`; unset seeds are random per process.
