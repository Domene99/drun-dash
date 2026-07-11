package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PolicyStore reads and writes the policies table.
type PolicyStore struct {
	db Querier
}

// NewPolicyStore returns a PolicyStore backed by db.
func NewPolicyStore(db Querier) *PolicyStore {
	return &PolicyStore{db: db}
}

const policyColumns = "id, org_id, name, description, current_version, created_at"

func scanPolicy(row pgx.Row) (Policy, error) {
	var p Policy
	err := row.Scan(&p.ID, &p.OrgID, &p.Name, &p.Description, &p.CurrentVersion, &p.CreatedAt)
	return p, err
}

// Create inserts a policy named name under orgID and returns the stored row,
// with current_version starting at 0. A nil description stores NULL. A
// duplicate (org_id, name) yields [ErrConflict].
func (s *PolicyStore) Create(ctx context.Context, orgID, name string, description *string) (Policy, error) {
	const q = `INSERT INTO policies (org_id, name, description) VALUES ($1, $2, $3) RETURNING ` + policyColumns
	policy, err := scanPolicy(s.db.QueryRow(ctx, q, orgID, name, description))
	if err != nil {
		return Policy{}, fmt.Errorf("store: create policy %q: %w", name, mapError(err))
	}
	return policy, nil
}

// GetByID returns the policy with the given id, or [ErrNotFound].
func (s *PolicyStore) GetByID(ctx context.Context, id string) (Policy, error) {
	const q = `SELECT ` + policyColumns + ` FROM policies WHERE id = $1`
	policy, err := scanPolicy(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Policy{}, fmt.Errorf("store: policy by id %q: %w", id, mapError(err))
	}
	return policy, nil
}

// ListByOrg returns every policy belonging to orgID, ordered by name.
func (s *PolicyStore) ListByOrg(ctx context.Context, orgID string) ([]Policy, error) {
	const q = `SELECT ` + policyColumns + ` FROM policies WHERE org_id = $1 ORDER BY name`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list policies for org %q: %w", orgID, mapError(err))
	}
	policies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Policy, error) {
		return scanPolicy(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list policies for org %q: %w", orgID, mapError(err))
	}
	return policies, nil
}

// SetCurrentVersion updates the current_version pointer on the policy with the
// given id and returns the updated row. It returns [ErrNotFound] if no such
// policy exists. Callers are responsible for having created the referenced
// [PolicyVersion] first.
func (s *PolicyStore) SetCurrentVersion(ctx context.Context, id string, version int) (Policy, error) {
	const q = `UPDATE policies SET current_version = $2 WHERE id = $1 RETURNING ` + policyColumns
	policy, err := scanPolicy(s.db.QueryRow(ctx, q, id, version))
	if err != nil {
		return Policy{}, fmt.Errorf("store: set policy %q current version=%d: %w", id, version, mapError(err))
	}
	return policy, nil
}
