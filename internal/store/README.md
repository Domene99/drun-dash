# internal/store

The PostgreSQL persistence layer for drun-dash: small, hand-written
repositories that map the control-plane schema to Go types.

This package covers the **tenancy + identity** tables (migrations
`000001`–`000002`) and the **agent-catalog domain** — model infrastructure,
billing, and the catalog itself (migrations `000003`–`000004`). The tenancy
repositories are the **reference pattern** every other store copies, so
consistency across the package matters more than breadth. The runtime /
telemetry tables (`000005`–`000006`) are intentionally not covered here.

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

The row structs live in per-domain `models_*.go` files: `models.go` (tenancy +
identity), `models_modelinfra.go`, `models_billing.go`, and
`models_catalog.go`.

## Types & conventions

- **UUIDs** are `string`; pgx scans and encodes them transparently.
- **`TIMESTAMPTZ`** columns are `time.Time`.
- **Nullable** columns are pointer fields (e.g. `Policy.Description`,
  `Entitlement.MemberID`), except cap amounts (see below).
- **Enums** are typed `string` constants with a `Valid()` method, mirroring
  `Role`: `BudgetScope` (`organization` / `team` / `agent` / `member`) and
  `BudgetWindow` (`daily` / `weekly` / `monthly`).
- **`jsonb`** columns (`match_json`, `content_json`, `allowed_tools`,
  `signing_json`) are `json.RawMessage`; a `nil` value means SQL NULL, and the
  columns with a default (`match_json`, `allowed_tools`) fall back to it via
  `COALESCE` when nil is passed.
- **`numeric(20,6)`** cap amounts (`budget_rules.soft_cap` / `hard_cap`) are
  `pgtype.Numeric` (part of pgx, not a new dependency) so exact decimals
  round-trip without floating-point loss; an invalid value (`Valid == false`)
  represents SQL NULL.
- **Create** takes explicit column values (a `Create…Params` struct for the
  wider inserts) and returns the fully-populated stored row via `RETURNING`, so
  callers see database-assigned `id`, `created_at`, and defaults.
- **`entitlements.Create`** validates the member-XOR-team rule in Go *before*
  the insert, returning a descriptive error (not a store sentinel) when both or
  neither principal is set, mirroring the database CHECK constraint.

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

Pure logic (error translation, `Role.Valid`, the enum `Valid()` methods, and
the entitlement member-XOR-team pre-check) is covered by `store_test.go` and
`catalog_test.go`, which run with **no database**:

```sh
go test ./internal/store/...
```

The database-touching tests live in `integration_test.go` and
`catalog_integration_test.go` behind the `integration` build tag and **skip**
when Postgres is unreachable (mirroring `internal/db`):

```sh
# Uses config.DefaultDatabaseURL (local Docker dev stack) unless DATABASE_URL is set.
go test -tags integration ./internal/store/...
```

Each integration test creates a throwaway organization and deletes it on
cleanup; deletion cascades to every dependent row, so tests stay isolated and
leave no residue.
