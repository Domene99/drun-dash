# internal/store

The PostgreSQL persistence layer for drun-dash: small, hand-written
repositories that map the control-plane schema to Go types.

This package covers the **tenancy + identity** tables (migrations
`000001`–`000002`), the **agent-catalog domain** — model infrastructure,
billing, and the catalog itself (migrations `000003`–`000004`) — and the
**runtime + telemetry domain** — launched sessions, scoped proxy credentials,
append-only usage metering, the approval queue, and ingested trajectories
(migrations `000005`–`000006`). It also exposes the read-only **analytics
aggregation** queries (spend rollups and budget utilisation) that the analytics
endpoints build on. The tenancy repositories are the **reference pattern** every
other store copies, so consistency across the package matters more than breadth.

## The pattern

- **One repository per aggregate.** Each table (or join table) gets a `…Store`
  type constructed with a `New…Store(db Querier)` function.
- **`Querier`, not a concrete pool.** Repositories depend on the narrow
  [`Querier`](store.go) interface — the subset of the pgx API they use
  (`Query`, `QueryRow`, `Exec`). A `*db.Pool` satisfies it directly (it embeds
  `*pgxpool.Pool`), and so does a `pgx.Tx`, so the same methods run inside or
  outside a caller-managed transaction.
- **Hand-written SQL** with positional parameters (`$1`, `$2`, …). No ORM, no
  query builder.
- **Errors never leak the driver.** Lookups translate `pgx.ErrNoRows` to the
  sentinel `ErrNotFound`; unique-constraint violations become `ErrConflict`
  (naming the offending constraint). Branch on them with `errors.Is`:

  ```go
  org, err := orgs.GetBySlug(ctx, "acme")
  if errors.Is(err, store.ErrNotFound) {
      // 404
  }
  ```

- **Context first, wrapped errors.** Every method takes `context.Context` and
  wraps failures with operation context via `%w`.

## Stores

### Tenancy + identity (`000001`–`000002`)

| File | Type | Methods |
|------|------|---------|
| `organizations.go` | `OrganizationStore` | `Create`, `GetByID`, `GetBySlug`, `List` |
| `environments.go` | `EnvironmentStore` | `Create`, `GetByID`, `ListByOrg` |
| `teams.go` | `TeamStore` | `Create`, `GetByID`, `ListByOrg` |
| `members.go` | `MemberStore` | `Create`, `GetByID`, `GetByIDPSubject`, `ListByOrg`, `SetActive` |
| `memberships.go` | `TeamMembershipStore` | `Add`, `Remove`, `ListMembers`, `ListTeams` |

### Model infrastructure (`000003`)

| File | Type | Methods |
|------|------|---------|
| `model_endpoints.go` | `ModelEndpointStore` | `Create`, `GetByID`, `ListByOrg`, `SetActive` |
| `routing_rules.go` | `RoutingRuleStore` | `Create`, `ListByOrg` (by priority), `Delete` |

### Billing (`000004`)

| File | Type | Methods |
|------|------|---------|
| `budgets.go` | `BudgetStore` | `Create`, `GetByID`, `ListByOrg` |
| `budget_rules.go` | `BudgetRuleStore` | `Create`, `ListByBudget`, `Delete` |

### Catalog (`000004`)

| File | Type | Methods |
|------|------|---------|
| `policies.go` | `PolicyStore` | `Create`, `GetByID`, `ListByOrg`, `SetCurrentVersion` |
| `policy_versions.go` | `PolicyVersionStore` | `Create`, `GetByPolicyAndVersion`, `ListByPolicy` |
| `agents.go` | `AgentStore` | `Create`, `GetByID`, `ListByOrg`, `SetCurrentVersion` |
| `agent_versions.go` | `AgentVersionStore` | `Create`, `GetByAgentAndVersion`, `ListByAgent` |
| `entitlements.go` | `EntitlementStore` | `Create`, `ListByAgent`, `ListByMember`, `ListByTeam`, `Delete` |

### Runtime + metering (`000005`)

| File | Type | Methods |
|------|------|---------|
| `sessions.go` | `SessionStore` | `Create`, `GetByID`, `ListByOrg`, `ListByAgent`, `SetStatus` |
| `credential_grants.go` | `CredentialGrantStore` | `Create`, `GetByTokenHash`, `ListBySession`, `Revoke` |
| `usage_events.go` | `UsageEventStore` | `Append`, `ListByOrg`, `ListBySession` |
| `approval_requests.go` | `ApprovalRequestStore` | `Create`, `GetByID`, `ListByOrg`, `ListPendingByOrg`, `Decide` |

### Telemetry (`000006`)

| File | Type | Methods |
|------|------|---------|
| `trajectories.go` | `TrajectoryStore` | `Create`, `GetByID`, `ListByOrg`, `SetEnded` |
| `trajectory_nodes.go` | `TrajectoryNodeStore` | `Create`, `GetByID`, `GetByEventID`, `ListByTrajectory` |

### Analytics (aggregation over `000004`–`000005`)

| File | Type | Methods |
|------|------|---------|
| `analytics.go` | `AnalyticsStore` | `SpendByOrg` (spend + tokens grouped by agent / policy / principal), `BudgetUtilization` (current-window spend vs. hard-cap per budget rule) |

`AnalyticsStore` owns no table — every method is a `GROUP BY` / `SUM` read over
`usage_events` (joined through `sessions` / `policy_versions` for the policy and
principal groupings) and `budgets` / `budget_rules`. Its result rows
(`SpendSummary`, `SpendGroupRow`, `BudgetUsageRow`) live inline in `analytics.go`
rather than a `models_*.go` file, since they are query shapes, not table rows.

The row structs live in per-domain `models_*.go` files: `models.go` (tenancy +
identity), `models_modelinfra.go`, `models_billing.go`, `models_catalog.go`,
`models_runtime.go`, and `models_telemetry.go`.

## Types & conventions

- **UUIDs** are `string`; pgx scans and encodes them transparently. The one
  exception is `usage_events.id`, a `BIGINT` identity, scanned as `int64`.
- **`TIMESTAMPTZ`** columns are `time.Time`.
- **Nullable** columns are pointer fields (e.g. `Policy.Description`,
  `Entitlement.MemberID`, `Session.TerminatedAt`), except cap amounts (see
  below).
- **Enums** are typed `string` constants with a `Valid()` method, mirroring
  `Role`: `BudgetScope`, `BudgetWindow`, `SessionStatus`
  (`pending` / `active` / `terminated` / `failed`), `ApprovalStatus`
  (`pending` / `approved` / `denied` / `expired`), and `TrajectoryFieldClass`
  (`structure` / `payload`).
- **`jsonb`** columns (`match_json`, `content_json`, `allowed_tools`,
  `signing_json`, `labels`, `scopes`, `context_json`, `metadata_json`) are
  `json.RawMessage`; a `nil` value passed to a `Create` falls back to the
  column default (`'{}'` or `'[]'`) via `COALESCE`.
- **`numeric(20,6)`** amounts (`budget_rules.soft_cap` / `hard_cap`,
  `usage_events.cost_amount`) are `pgtype.Numeric` (part of pgx, not a new
  dependency) so exact decimals round-trip without floating-point loss; for the
  nullable caps an invalid value (`Valid == false`) represents SQL NULL, while
  the non-null `cost_amount` treats an invalid value as 0.
- **Create** takes explicit column values (a `Create…Params` struct for the
  wider inserts) and returns the fully-populated stored row via `RETURNING`, so
  callers see database-assigned `id`, `created_at`, and defaults.
- **Generated columns** are never inserted: `usage_events.total_tokens` is
  `GENERATED ALWAYS` (`prompt_tokens + completion_tokens`) and read-only, so
  `Append` omits it from the INSERT and reads it back via `RETURNING`.
- **Enum defaults are applied in Go** where the column has one: `Session.Status`
  defaults to `pending` and `TrajectoryNode.FieldClass` to `structure` when the
  param is empty, matching the column default.
- **Security:** `credential_grants` stores only the **hash** of a proxy token
  (`proxy_token_hash`); the raw token is never persisted, and `GetByTokenHash`
  is the proxy's per-call lookup.
- **`entitlements.Create`** validates the member-XOR-team rule in Go *before*
  the insert, returning a descriptive error (not a store sentinel) when both or
  neither principal is set, mirroring the database CHECK constraint.
  **`approval_requests.Decide`** likewise rejects a non-terminal status before
  the DB and only acts on a still-pending row (a second decision on the same id
  returns `ErrNotFound`).

## Usage

```go
pool, _ := db.Connect(ctx, cfg)
orgs := store.NewOrganizationStore(pool)

org, err := orgs.Create(ctx, "acme", "Acme Inc")
members := store.NewMemberStore(pool)
m, err := members.Create(ctx, store.CreateMemberParams{
    OrgID:      org.ID,
    IDPSubject: "auth0|abc123",
    Email:      "ada@example.com",
    Role:       store.RoleAdmin,
})
```

## Tests

Pure logic (error translation, `Role.Valid`, the enum `Valid()` methods, the
entitlement member-XOR-team pre-check, and the approval terminal-status
pre-check) is covered by `store_test.go`, `catalog_test.go`, and
`runtime_test.go`, which run with **no database**:

```sh
go test ./internal/store/...
```

The database-touching tests live in `integration_test.go`,
`catalog_integration_test.go`, `runtime_integration_test.go`,
`telemetry_integration_test.go`, and `analytics_test.go` behind the
`integration` build tag and **skip** when Postgres is unreachable (mirroring
`internal/db`):

```sh
# Uses config.DefaultDatabaseURL (local Docker dev stack) unless DATABASE_URL is set.
go test -tags integration ./internal/store/...
```

Each integration test creates a throwaway organization and deletes it on
cleanup; deletion cascades to every dependent row, so tests stay isolated and
leave no residue.
