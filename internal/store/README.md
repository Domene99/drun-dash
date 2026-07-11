# internal/store

The PostgreSQL persistence layer for drun-dash: small, hand-written
repositories that map the control-plane schema to Go types.

This package covers the **tenancy + identity** tables only (migrations
`000001`–`000002`). It is the **reference pattern** that every later store
package — catalog, budgets, telemetry — copies, so consistency here matters
more than breadth.

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

| File | Type | Methods |
|------|------|---------|
| `organizations.go` | `OrganizationStore` | `Create`, `GetByID`, `GetBySlug`, `List` |
| `environments.go` | `EnvironmentStore` | `Create`, `GetByID`, `ListByOrg` |
| `teams.go` | `TeamStore` | `Create`, `GetByID`, `ListByOrg` |
| `members.go` | `MemberStore` | `Create`, `GetByID`, `GetByIDPSubject`, `ListByOrg`, `SetActive` |
| `memberships.go` | `TeamMembershipStore` | `Add`, `Remove`, `ListMembers`, `ListTeams` |

`models.go` holds the row structs (`Organization`, `Environment`, `Team`,
`Member`, `TeamMembership`) and the `Role` type — a typed string mirroring the
`member_role` enum, with the `RoleAdmin` / `RoleMember` / `RoleAuditor`
constants.

## Types & conventions

- **UUIDs** are `string`; pgx scans and encodes them transparently.
- **`TIMESTAMPTZ`** columns are `time.Time`.
- **Nullable** columns (only `members.display_name` here) are pointer fields.
- **Create** takes explicit column values (a `CreateMemberParams` struct for
  the wider `members` insert) and returns the fully-populated stored row via
  `RETURNING`, so callers see the database-assigned `id` and `created_at`.

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

Pure logic (error translation, `Role.Valid`) is covered by `store_test.go`,
which runs with **no database**:

```sh
go test ./internal/store/...
```

The database-touching tests live in `integration_test.go` behind the
`integration` build tag and **skip** when Postgres is unreachable (mirroring
`internal/db`):

```sh
# Uses config.DefaultDatabaseURL (local Docker dev stack) unless DATABASE_URL is set.
go test -tags integration ./internal/store/...
```

Each integration test creates a throwaway organization and deletes it on
cleanup; deletion cascades to every dependent row, so tests stay isolated and
leave no residue.
