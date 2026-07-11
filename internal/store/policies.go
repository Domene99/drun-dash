package store

import (
	"context"
	"fmt"
	"time"

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

// ListByOrgPage returns up to limit policies for orgID ordered by (created_at,
// id), starting strictly after the (afterCreatedAt, afterID) position. A zero
// afterCreatedAt (afterID ignored) starts from the beginning of the list. The
// caller typically passes limit+1 to detect whether another page exists and to
// derive the next cursor from the last kept row; this method only runs the seek
// and returns the rows. It is the keyset-paginated counterpart to [ListByOrg]
// (which orders by name and is left unchanged) per ADR-0002 §3.
//
// A non-positive limit is rejected as a programming error: the handler layer is
// the primary bound (parsing and clamping the query's limit to [1, 200]), so a
// limit of zero or below reaching the store is a bug, not a request for an
// empty page.
func (s *PolicyStore) ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]Policy, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list policies page for org %q: limit must be positive, got %d", orgID, limit)
	}
	q := `SELECT ` + policyColumns + ` FROM policies WHERE org_id = $1`
	args := []any{orgID, limit}
	if !afterCreatedAt.IsZero() {
		q += ` AND (created_at, id) > ($3, $4)`
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY created_at, id LIMIT $2`
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list policies page for org %q: %w", orgID, mapError(err))
	}
	policies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Policy, error) {
		return scanPolicy(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list policies page for org %q: %w", orgID, mapError(err))
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
