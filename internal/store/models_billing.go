package store

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// The structs and enums below map one-to-one to the billing tables created by
// migration 000004. Cap amounts are NUMERIC(20,6); they use pgtype.Numeric
// (part of pgx, not a new dependency) so exact decimal values round-trip
// without floating-point loss. An invalid pgtype.Numeric (Valid == false)
// represents SQL NULL.

// BudgetScope mirrors the Postgres budget_scope enum: what a [BudgetRule] caps
// spend for.
type BudgetScope string

// The budget_scope enum values.
const (
	BudgetScopeOrganization BudgetScope = "organization"
	BudgetScopeTeam         BudgetScope = "team"
	BudgetScopeAgent        BudgetScope = "agent"
	BudgetScopeMember       BudgetScope = "member"
)

// Valid reports whether s is one of the defined budget_scope values.
func (s BudgetScope) Valid() bool {
	switch s {
	case BudgetScopeOrganization, BudgetScopeTeam, BudgetScopeAgent, BudgetScopeMember:
		return true
	default:
		return false
	}
}

// BudgetWindow mirrors the Postgres budget_window enum: the rolling window a
// [BudgetRule] resets over.
type BudgetWindow string

// The budget_window enum values.
const (
	BudgetWindowDaily   BudgetWindow = "daily"
	BudgetWindowWeekly  BudgetWindow = "weekly"
	BudgetWindowMonthly BudgetWindow = "monthly"
)

// Valid reports whether w is one of the defined budget_window values.
func (w BudgetWindow) Valid() bool {
	switch w {
	case BudgetWindowDaily, BudgetWindowWeekly, BudgetWindowMonthly:
		return true
	default:
		return false
	}
}

// Budget is a named spend envelope for an organization (table budgets; unique
// per (org_id, name)). Its caps live in one or more [BudgetRule] rows.
type Budget struct {
	ID        string    // id (uuid)
	OrgID     string    // org_id → organizations.id
	Name      string    // name
	Currency  string    // currency (defaults to 'USD')
	CreatedAt time.Time // created_at
}

// BudgetRule is a scoped cap within a [Budget] (table budget_rules). soft_cap
// warns; hard_cap blocks. Either cap may be NULL (Valid == false); scope_id is
// an optional target the scope applies to (NULL means the whole scope).
type BudgetRule struct {
	ID        string         // id (uuid)
	BudgetID  string         // budget_id → budgets.id
	Scope     BudgetScope    // scope (budget_scope enum)
	ScopeID   *string        // scope_id (nullable uuid)
	Window    BudgetWindow   // window_period (budget_window enum)
	SoftCap   pgtype.Numeric // soft_cap (nullable numeric(20,6))
	HardCap   pgtype.Numeric // hard_cap (nullable numeric(20,6))
	CreatedAt time.Time      // created_at
}
