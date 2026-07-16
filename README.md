# drun-dash

**drun-dash** is a hosted **governance control plane** for the open-source
[drun](https://github.com/dmosc/drun) agent engine. drun runs locally on
developer machines; drun-dash sits in front of it to broker LLM credentials,
distribute signed policy, meter and cap spend, and capture agent trajectories
for audit. **The drun engine itself is never modified** — drun-dash integrates
with it only over the external `drun-mcp` endpoint.

It is a single Go monorepo: three server binaries, two CLIs, and a set of shared
libraries, all under one module (`github.com/Domene99/drun-dash`) and one
locked stack (Go 1.22 · chi · PostgreSQL 16 · Redis 7 · templ + HTMX). See
[docs/TECH_STACK.md](docs/TECH_STACK.md) and [docs/MONOREPO.md](docs/MONOREPO.md).

> **Status.** This is a v1 platform cut. The REST contract in `openapi/v1.yaml`
> is fully specified, most operations are implemented, and a few are honestly
> stubbed — see [Known gaps & follow-ups](#known-gaps--follow-ups). Kubernetes
> manifests and Terraform are out of v1 scope.

---

## What's in the box

### Services (hosted control plane)

| Binary | What it does |
| ------ | ------------ |
| **control-plane** (`cmd/control-plane`) | The REST API (`openapi/v1.yaml`) plus OIDC auth. Owns tenants, users, the agent catalog, versioned policy, signed policy bundles, governed **session launch**, the approvals queue, trajectory ingest, and spend analytics. Backed by PostgreSQL 16; Redis 7 for session/metering state. |
| **llm-proxy** (`cmd/llm-proxy`) | An OpenAI-compatible broker. Every model call routes through it: it validates the per-session credential grant, enforces the org's hard budget caps, injects the **master provider credential** (clients never hold it), reverse-proxies to the upstream, streams SSE back unbuffered, and meters spend. |
| **admin-ui** (`cmd/admin-ui`) | A server-rendered admin web app (Go + templ + HTMX, no SPA). Talks to the control-plane REST API over HTTP only. Pages: dashboard, Sessions (flight recorder), forensic **trajectory replay** viewer, Spend, and the Approvals queue. |

### CLIs

| Binary | Audience | What it does |
| ------ | -------- | ------------ |
| **drun-dash** (`cmd/drun-dash`) | Developers | Authenticates via the OAuth 2.0 device flow, then drives the control plane: `login`, `whoami`, `agents`, `policies`, `sessions launch`/`terminate`, `approvals`, `spend`, and `matrix run` (the parallel matrix engine). See [internal/cli/README.md](internal/cli/README.md). |
| **policyctl** (`cmd/policyctl`) | Operators | Signs and verifies **policy bundles** — Ed25519 `keygen`, `sign`, and `verify`. The signing counterpart to the verification-only `pkg/policy` library. See [cmd/policyctl/README.md](cmd/policyctl/README.md). |

Shared libraries live under `pkg/`: `pkg/policy` (signed-bundle load/verify),
`pkg/trajectory` (trajectory event schema + residency validation), and
`pkg/drunmcp` (the `drun-mcp` client).

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│  drun-dash CONTROL PLANE (hosted)                                     │
│                                                                       │
│   ┌────────────────┐     ┌──────────────┐     ┌─────────────────┐     │
│   │ control-plane  │     │  llm-proxy   │     │   admin-ui      │     │
│   │ REST API +auth │     │ OpenAI-compat│     │ (templ + HTMX)  │     │
│   │ catalog/policy │     │ credential   │     │ dashboard,      │     │
│   │ launch/approve │     │ broker +     │     │ sessions,       │     │
│   │ trajectory     │     │ metering     │     │ spend, approvals│     │
│   └───────┬────────┘     └──────┬───────┘     └────────┬────────┘     │
│           │        PostgreSQL 16  ·  Redis 7           │              │
└───────────┼─────────────────────┼─────────────────────┼──────────────┘
            │ signed policy bundle │ metered LLM traffic  │ REST (HTTP)
            │ + scoped proxy_token │ (master cred injected)
            ▼                      ▲
┌─────────────────────────────────────────────────────────────────────┐
│  DEVELOPER CLIENT (local machine)                                     │
│                                                                       │
│   drun-dash CLI ──► drun engine (unmodified) ──► drun-mcp (Rust)      │
│                                                   MCP Streamable HTTP  │
│                                                   127.0.0.1:7273/mcp   │
└─────────────────────────────────────────────────────────────────────┘
```

A fuller data-flow walkthrough (launch → run → meter → capture) is in
[docs/OVERVIEW.md](docs/OVERVIEW.md); the canonical architecture reference is
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

---

## Quickstart (local, container-based)

Bring up the whole platform — Postgres + Redis + one-shot migrations + the three
services — with one command. Full details in
[docs/RUNBOOK.md](docs/RUNBOOK.md).

**Prerequisites:** Docker Engine with the Compose v2 plugin. (No local Go
toolchain needed — each service is built inside a multi-stage image.)

```bash
cd deploy/docker

# 1. Create your env file (defaults are fine for a first local run).
cp .env.app.example .env.app

# 2. Build the images and bring the stack up.
docker compose --env-file .env.app -f docker-compose.app.yml up --build -d

# 3. Watch startup / health.
docker compose --env-file .env.app -f docker-compose.app.yml ps
```

Startup order is enforced with healthchecks + `depends_on`: Postgres/Redis
become healthy, `migrate` applies all migrations once and exits, then
control-plane (`:8080`), llm-proxy (`:8081`), and admin-ui (`:8082`) come up.

**Sign up your organization.** A fresh install starts **empty** — there is no
demo org. Open the admin UI at <http://localhost:8082> and **sign up your
organization**; the first account becomes the **Owner** (super-admin). From
there you create agents and invite members. This is the normal path — no seed
step is required to use the app.

**Optional — seed demo data.** Want a ready-made demo org instead (environments,
teams, members, endpoints, a policy, a budget, agents, and their RBAC role
grants)? Load the **optional**, dev-only seed from the repo root against the
running Postgres. It is idempotent — a shortcut for demos/tests, not a
requirement:

```bash
export DATABASE_URL='postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable'
psql "$DATABASE_URL" -f deploy/seed/seed.sql   # or: make seed
```

**Use the developer CLI.** Build it (`go build ./cmd/drun-dash`, or `make build`
for all binaries), then log in and explore:

```bash
drun-dash --server http://localhost:8080 login   # OAuth 2.0 device flow
drun-dash whoami
drun-dash agents list
drun-dash sessions launch --agent <agentId>      # prints a one-time proxy_token
drun-dash spend --group-by agent
```

> **Auth note.** The control plane mounts its auth routes (and therefore `login`)
> only when `CP_OIDC_ISSUER` / `CP_OIDC_CLIENT_ID` point at a reachable IdP;
> otherwise it serves the health probes only. Set them in `.env.app` to enable
> `drun-dash login` end-to-end. See the [RUNBOOK](docs/RUNBOOK.md) env reference.

The admin UI is at <http://localhost:8082> — paste a control-plane session token
on its login page to view live governance data.

---

## Security model

- **Credential custody.** Provider API keys live **only** in the control plane.
  The llm-proxy injects the master credential per call and **overwrites** any
  client-supplied `Authorization`. Developers and CLIs never hold raw provider
  keys — they hold a scoped, per-session **`proxy_token`** minted at launch,
  validated fail-closed on every call (revoked / expired / terminated / wrong
  scope → rejected). See [ADR-0003](docs/adr/0003-session-launch-and-credential-grant.md).
- **Signed policy bundles.** Policy boundaries are versioned and immutable. A
  launch hands the client an **Ed25519-signed** bundle (`content_hash` +
  `signature`), verified by `pkg/policy` before use. `policyctl` signs bundles;
  the control plane and clients only ever verify. See
  [ADR-0001](docs/adr/0001-authentication-and-session-model.md).
- **Budget enforcement.** The proxy checks the org's hard budget caps **before**
  injecting any credential; a call over cap returns `429 budget_exceeded` and
  never contacts the upstream (so it is never metered). The check fails closed.
- **Data residency.** Trajectory ingest accepts tree **structure + metadata** by
  default and **rejects raw payloads** (`field_class: payload`) unless a caller
  explicitly opts into an authorized payload flow (`pkg/trajectory`). This keeps
  file contents off the control plane by default.
- **Trust boundary.** Local drun ↔ drun-dash traffic stays on `127.0.0.1` via
  `drun-mcp`; trajectory metadata flows outbound for metering and audit.

---

## Repository layout

```
cmd/            control-plane · llm-proxy · admin-ui · drun-dash · policyctl
internal/       private app packages (auth, proxy, httpapi, ui, matrix, store, …)
pkg/            importable libs: policy, trajectory, drunmcp
openapi/        v1.yaml — hand-written REST contract (Spectral-linted)
migrations/     golang-migrate SQL migrations
deploy/         docker/ (compose + Dockerfiles) and seed/ (dev seed SQL)
docs/           ARCHITECTURE, OVERVIEW, MONOREPO, TECH_STACK, RUNBOOK, adr/
```

Canonical checks: `go build ./...`, `go test ./...`, `gofmt`, and
`spectral lint openapi/v1.yaml` — all run in CI (`.github/workflows/ci.yml`).
`make build` compiles every binary into `./bin`.

---

## Known gaps & follow-ups

Documenting what shipped honestly — these are real, tracked limitations, not
hidden ones:

- **`publishAgentVersion` → `501`.** `POST /agents/{agentId}/versions` returns
  `501 not_implemented`: the `AgentVersionCreate` schema carries only a
  `manifest_digest`, not the manifest fields the store requires. Agent versions
  are seeded/read but not yet published via the API.
- **Org-scoped budget caps only.** The proxy enforces caps at the **org** level;
  per-member / per-agent / per-team scoping exists in the store but is not yet
  narrowed in the request-path check.
- **Matrix engine launches N sessions.** Each matrix branch is an independent
  governed session. The drun **checkpoint-fork** optimization (fork all branches
  from one shared checkpoint) is a documented follow-up.
- **Trajectory payload capture is off by default.** Only structure/metadata is
  ingested; the authorized payload-bearing ingest flow is stubbed behind the
  residency opt-in.
- **CLI policy sync / drun engine control are stubs.** `launch` displays /
  `--json`-dumps the bundle; writing it to disk and configuring the local engine
  is deferred (marked with `// TODO(batch/…)`).
- **Admin UI is read-heavy.** It surfaces live governance data and approval
  decisions; broader write/management flows are future work.

---

## Documentation

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — the three planes and trust boundaries.
- **[docs/OVERVIEW.md](docs/OVERVIEW.md)** — end-to-end data-flow walkthrough.
- **[docs/RUNBOOK.md](docs/RUNBOOK.md)** — full-stack local deployment, ports, env, seeding.
- **[docs/adr/](docs/adr/README.md)** — Architecture Decision Records (auth/session, REST conventions, session launch & credential grant).
- **[openapi/v1.yaml](openapi/v1.yaml)** — the REST API contract.
- **[docs/TECH_STACK.md](docs/TECH_STACK.md)** · **[docs/MONOREPO.md](docs/MONOREPO.md)** — the locked stack and module layout.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
