package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// BudgetStore reads and writes the budgets table.
type BudgetStore struct {
	db Querier
}

// NewBudgetStore returns a BudgetStore backed by db.
func NewBudgetStore(db Querier) *BudgetStore {
	return &BudgetStore{db: db}
}

const budgetColumns = "id, org_id, name, currency, created_at"

func scanBudget(row pgx.Row) (Budget, error) {
	var b Budget
	err := row.Scan(&b.ID, &b.OrgID, &b.Name, &b.Currency, &b.CreatedAt)
	return b, err
}

// Create inserts a budget named name under orgID and returns the stored row. An
// empty currency defaults to 'USD', matching the column default. A duplicate
// (org_id, name) yields [ErrConflict].
func (s *BudgetStore) Create(ctx context.Context, orgID, name, currency string) (Budget, error) {
	if currency == "" {
		currency = "USD"
	}
	const q = `INSERT INTO budgets (org_id, name, currency) VALUES ($1, $2, $3) RETURNING ` + budgetColumns
	budget, err := scanBudget(s.db.QueryRow(ctx, q, orgID, name, currency))
	if err != nil {
		return Budget{}, fmt.Errorf("store: create budget %q: %w", name, mapError(err))
	}
	return budget, nil
}

// GetByID returns the budget with the given id, or [ErrNotFound].
func (s *BudgetStore) GetByID(ctx context.Context, id string) (Budget, error) {
	const q = `SELECT ` + budgetColumns + ` FROM budgets WHERE id = $1`
	budget, err := scanBudget(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Budget{}, fmt.Errorf("store: budget by id %q: %w", id, mapError(err))
	}
	return budget, nil
}

// ListByOrg returns every budget belonging to orgID, ordered by name.
func (s *BudgetStore) ListByOrg(ctx context.Context, orgID string) ([]Budget, error) {
	const q = `SELECT ` + budgetColumns + ` FROM budgets WHERE org_id = $1 ORDER BY name`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list budgets for org %q: %w", orgID, mapError(err))
	}
	budgets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Budget, error) {
		return scanBudget(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list budgets for org %q: %w", orgID, mapError(err))
	}
	return budgets, nil
}
