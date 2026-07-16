package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PolicyRoleStore reads and writes the policy_roles join table (ADR-0010): the
// set of roles a policy permits. Under the policy-centric access model an agent
// governed by a policy is visible and runnable to a member iff one of the
// member's reachable roles is in that policy's permitted set (an agent with no
// policy is general access; an all_agents role sees everything). It supersedes
// the role_agents / entitlements grant tables.
type PolicyRoleStore struct {
	db Querier
}

// NewPolicyRoleStore returns a PolicyRoleStore backed by db.
func NewPolicyRoleStore(db Querier) *PolicyRoleStore {
	return &PolicyRoleStore{db: db}
}

// SetRoles replaces the full set of roles permitted by policyID with roleIDs, in
// one transaction: it deletes the policy's existing rows then inserts the new
// set. Duplicate ids in roleIDs are collapsed. An id that does not reference an
// existing role is rejected by the role_id foreign key (surfaced as the wrapped
// error); the caller validates org ownership before calling. Passing an empty
// roleIDs clears the policy's permitted roles.
func (s *PolicyRoleStore) SetRoles(ctx context.Context, policyID string, roleIDs []string) error {
	unique := dedupeStrings(roleIDs)

	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("store: set roles for policy %q: %w", policyID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM policy_roles WHERE policy_id = $1`, policyID); err != nil {
		return fmt.Errorf("store: set roles for policy %q: %w", policyID, mapError(err))
	}
	const ins = `INSERT INTO policy_roles (policy_id, role_id) VALUES ($1, $2)`
	for _, roleID := range unique {
		if _, err := tx.Exec(ctx, ins, policyID, roleID); err != nil {
			return fmt.Errorf("store: set roles for policy %q: %w", policyID, mapError(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: set roles for policy %q: %w", policyID, mapError(err))
	}
	return nil
}

// ListRoles returns the ids of the roles permitted by policyID, ordered by
// role_id for a stable result. An empty result is not an error.
func (s *PolicyRoleStore) ListRoles(ctx context.Context, policyID string) ([]string, error) {
	const q = `SELECT role_id FROM policy_roles WHERE policy_id = $1 ORDER BY role_id`
	rows, err := s.db.Query(ctx, q, policyID)
	if err != nil {
		return nil, fmt.Errorf("store: list roles for policy %q: %w", policyID, mapError(err))
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: list roles for policy %q: %w", policyID, mapError(err))
	}
	return ids, nil
}

// ListPoliciesForRoles returns the distinct ids of the policies permitted to any
// of roleIDs, ordered by policy_id for a stable result. An empty roleIDs yields
// an empty slice without touching the database.
func (s *PolicyRoleStore) ListPoliciesForRoles(ctx context.Context, roleIDs []string) ([]string, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT DISTINCT policy_id FROM policy_roles WHERE role_id = ANY($1) ORDER BY policy_id`
	rows, err := s.db.Query(ctx, q, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list policies for roles: %w", mapError(err))
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: list policies for roles: %w", mapError(err))
	}
	return ids, nil
}

// begin opens a transaction on the underlying pool. It mirrors
// RoleStore.begin — both *pgxpool.Pool and pgx.Tx satisfy Querier, but only a
// pool (or an outer tx) can begin a nested one, so SetRoles requires a
// transaction-capable Querier.
func (s *PolicyRoleStore) begin(ctx context.Context) (pgx.Tx, error) {
	tb, ok := s.db.(txBeginner)
	if !ok {
		return nil, fmt.Errorf("db does not support transactions")
	}
	return tb.Begin(ctx)
}

// dedupeStrings returns the input with duplicates removed, preserving first
// appearance order. A nil/empty input returns nil.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
