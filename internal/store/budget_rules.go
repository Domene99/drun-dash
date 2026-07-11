package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// BudgetRuleStore reads and writes the budget_rules table.
type BudgetRuleStore struct {
	db Querier
}

// NewBudgetRuleStore returns a BudgetRuleStore backed by db.
func NewBudgetRuleStore(db Querier) *BudgetRuleStore {
	return &BudgetRuleStore{db: db}
}

const budgetRuleColumns = "id, budget_id, scope, scope_id, window_period, soft_cap, hard_cap, created_at"

func scanBudgetRule(row pgx.Row) (BudgetRule, error) {
	var r BudgetRule
	err := row.Scan(&r.ID, &r.BudgetID, &r.Scope, &r.ScopeID, &r.Window, &r.SoftCap, &r.HardCap, &r.CreatedAt)
	return r, err
}

// CreateBudgetRuleParams carries the fields for [BudgetRuleStore.Create]. A nil
// ScopeID applies the rule across the whole scope; an invalid SoftCap/HardCap
// (Valid == false) stores SQL NULL.
type CreateBudgetRuleParams struct {
	BudgetID string
	Scope    BudgetScope
	ScopeID  *string
	Window   BudgetWindow
	SoftCap  pgtype.Numeric
	HardCap  pgtype.Numeric
}

// Create inserts a budget rule and returns the stored row. The database
// enforces the enum domains for scope/window and the soft_cap ≤ hard_cap
// invariant; callers can pre-check the enums with [BudgetScope.Valid] and
// [BudgetWindow.Valid].
func (s *BudgetRuleStore) Create(ctx context.Context, p CreateBudgetRuleParams) (BudgetRule, error) {
	const q = `INSERT INTO budget_rules (budget_id, scope, scope_id, window_period, soft_cap, hard_cap)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING ` + budgetRuleColumns
	rule, err := scanBudgetRule(s.db.QueryRow(ctx, q, p.BudgetID, p.Scope, p.ScopeID, p.Window, p.SoftCap, p.HardCap))
	if err != nil {
		return BudgetRule{}, fmt.Errorf("store: create budget rule for budget %q: %w", p.BudgetID, mapError(err))
	}
	return rule, nil
}

// ListByBudget returns every rule belonging to budgetID, ordered by scope then
// created_at for a stable order.
func (s *BudgetRuleStore) ListByBudget(ctx context.Context, budgetID string) ([]BudgetRule, error) {
	const q = `SELECT ` + budgetRuleColumns + ` FROM budget_rules WHERE budget_id = $1 ORDER BY scope, created_at, id`
	rows, err := s.db.Query(ctx, q, budgetID)
	if err != nil {
		return nil, fmt.Errorf("store: list budget rules for budget %q: %w", budgetID, mapError(err))
	}
	rules, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BudgetRule, error) {
		return scanBudgetRule(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list budget rules for budget %q: %w", budgetID, mapError(err))
	}
	return rules, nil
}

// Delete removes the budget rule with the given id. It returns [ErrNotFound] if
// no such rule exists.
func (s *BudgetRuleStore) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM budget_rules WHERE id = $1`
	tag, err := s.db.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: delete budget rule %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete budget rule %q: %w", id, ErrNotFound)
	}
	return nil
}
