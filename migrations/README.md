# Database migrations

PostgreSQL 16 schema for the drun-dash control plane, managed with
[`golang-migrate`](https://github.com/golang-migrate/migrate).

Migrations are plain SQL files using the golang-migrate naming convention:

```
migrations/{version}_{name}.up.sql    -- applied going forward
migrations/{version}_{name}.down.sql  -- rolls the same step back
```

Versions are sequential from `000001`. **All migration files are owned by a
single work stream** to keep the numbering conflict-free — add new steps by
appending the next number, never by inserting or renumbering.

The application-side migration runner (embedding these files and running them on
startup) is added in a later batch. This directory is just the SQL.

## The schema at a glance

| Migration | Domain | Tables |
|-----------|--------|--------|
| `000001_core_tenancy` | Tenancy | `organizations`, `environments` |
| `000002_identity` | Identity | `teams`, `members`, `team_memberships` |
| `000003_model_infrastructure` | Model routing | `model_endpoints`, `routing_rules` |
| `000004_agent_catalog` | Agent catalog | `policies`, `policy_versions`, `budgets`, `budget_rules`, `agents`, `agent_versions`, `entitlements` |
| `000005_runtime_metering` | Runtime & metering | `sessions`, `credential_grants`, `usage_events`, `approval_requests` |
| `000006_telemetry` | Telemetry | `trajectories`, `trajectory_nodes` |
| `000007_rbac` | RBAC | `roles`, `role_permissions`, `role_agents`, `member_roles`, `team_roles` |

Notes on the data model:

- **Tenancy:** every table is scoped to an `organizations` row (directly via
  `org_id`, or transitively through a parent). Deleting an organization cascades
  to its dependent rows.
- **Versioning:** `policies`/`policy_versions` and `agents`/`agent_versions`
  keep an append-only version history; the parent row's `current_version` points
  at the newest version. Published `agent_versions` pin a `policy_version` and
  `model_endpoint` with `ON DELETE RESTRICT` so live references cannot dangle.
- **Entitlements** grant an agent to **exactly one** principal — a member *XOR* a
  team — enforced by the `entitlements_member_xor_team` check constraint plus
  partial unique indexes.
- **RBAC (000007)** adds org-scoped `roles` bundling permissions
  (`role_permissions`, TEXT validated against a Go catalog) and agent access
  (`all_agents` flag or `role_agents`, which carries the entitlement overrides),
  assigned to members (`member_roles`) and teams (`team_roles`). Per ADR-0006 the
  step seeds four built-in roles per org and backfills `members.role` +
  `entitlements` onto roles; both are retained (not dropped) for one release, so
  the step is additive and reversible.
- **Secrets** are never stored: `model_endpoints.secret_ref` is an external
  handle, and `credential_grants.proxy_token_hash` stores only a token hash.
- **`usage_events`** is append-only token metering, keyed by a `bigint` identity
  and indexed on `(org_id, occurred_at)` for spend/time-range analytics.
  `trajectories` is indexed the same way; `credential_grants` has a unique index
  on `proxy_token_hash` for proxy token lookup.
- **Trajectories** store *structure and metadata only* by default
  (`field_class = 'structure'`); raw payloads are not persisted here.

Each migration file wraps its statements in `BEGIN`/`COMMIT` so a failed step
leaves the database unchanged.

## Prerequisites

- A running PostgreSQL 16 instance. The local dev stack in
  [`deploy/docker`](../deploy/docker) provides one:

  ```sh
  cd deploy/docker
  cp .env.example .env
  docker compose up -d
  ```

- The `migrate` CLI with the Postgres driver:

  ```sh
  go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
  ```

## Connection string

Matches the defaults in `deploy/docker/.env.example`:

```sh
export DATABASE_URL='postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable'
```

## Applying migrations

From the repository root:

```sh
# Apply every pending migration.
migrate -path migrations -database "$DATABASE_URL" up

# Apply the next N migrations only.
migrate -path migrations -database "$DATABASE_URL" up 1

# Roll the most recent migration back.
migrate -path migrations -database "$DATABASE_URL" down 1

# Roll everything back (drops all schema).
migrate -path migrations -database "$DATABASE_URL" down -all

# Show the current version.
migrate -path migrations -database "$DATABASE_URL" version
```

`migrate` records applied versions in a `schema_migrations` table it manages.

### Recovering from a dirty state

If a migration fails midway, `migrate` marks the version *dirty* and refuses to
continue. Fix the underlying SQL, then force the version back to the last
known-good number and re-run:

```sh
migrate -path migrations -database "$DATABASE_URL" force <last_good_version>
migrate -path migrations -database "$DATABASE_URL" up
```

## Adding a migration

1. Pick the next sequential number (e.g. `000007`).
2. Create both `000007_<name>.up.sql` and `000007_<name>.down.sql`.
3. Make the `.down.sql` the exact inverse of the `.up.sql` (drop tables in
   reverse dependency order; drop any `CREATE TYPE` enums too).
4. Verify a full round trip against a scratch database:

   ```sh
   migrate -path migrations -database "$DATABASE_URL" up
   migrate -path migrations -database "$DATABASE_URL" down -all
   migrate -path migrations -database "$DATABASE_URL" up
   ```
