# Local development seed data

[`seed.sql`](seed.sql) populates a running drun-dash database with a realistic,
repeatable demo dataset for local development. It is **dev-only** and lives
separately from [`migrations/`](../../migrations) — it is never applied in
production.

## Prerequisites

- The schema must already be migrated (migrations `000001`–`000004` at least).
  See [`migrations/README.md`](../../migrations/README.md).
- A `psql` client and a connection string. The local dev stack in
  [`deploy/docker`](../docker) provides Postgres 16; its default URL is:

  ```sh
  export DATABASE_URL='postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable'
  ```

## Applying

From the repository root:

```sh
psql "$DATABASE_URL" -f deploy/seed/seed.sql
```

The script is **idempotent** — running it again is a no-op (it will refresh
mutable fields but never create duplicate rows), so it is safe to re-run after
schema changes or to reset demo values.

## What it creates

All rows belong to a single organization with `slug = 'demo'`:

| Domain      | Rows |
|-------------|------|
| Tenancy     | 1 org (`demo`), 2 environments (`production`, `staging`) |
| Identity    | 2 teams (`Platform`, `Research`), 3 members (`admin` / `member` / `auditor`), team memberships |
| Model infra | 2 endpoints (`anthropic-primary`, `openai-fallback`), 1 routing rule for `claude-*` |
| Policy      | 1 policy (`standard-dev`) + 1 version (minimal `drun_config`, placeholder `content_hash`) |
| Budget      | 1 budget (`demo-monthly`) + 1 org-wide monthly rule (`soft_cap` ≤ `hard_cap`) |
| Agents      | 2 agents (`code-reviewer`, `doc-writer`), each with a version 1 pinning the policy version, endpoint, and budget |
| Entitlements| `code-reviewer` → `Platform` team; `doc-writer` → `Marvin Member` |

## Notes

- **No real secrets.** `model_endpoints.secret_ref` values are opaque handles
  (e.g. `vault://dev/anthropic`), not provider API keys. The seeded policy
  version carries a placeholder `content_hash` and is not a signed bundle.
- **How idempotency works.** Foreign keys are resolved by natural-key lookup
  (org `slug`, `name`, `idp_subject`, …) rather than hard-coded UUIDs. Inserts
  use `ON CONFLICT` on the relevant unique key; the two tables without a natural
  unique key (`routing_rules`, `budget_rules`) are guarded with `NOT EXISTS`.
