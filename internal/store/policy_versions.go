package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PolicyVersionStore reads and writes the policy_versions table.
type PolicyVersionStore struct {
	db Querier
}

// NewPolicyVersionStore returns a PolicyVersionStore backed by db.
func NewPolicyVersionStore(db Querier) *PolicyVersionStore {
	return &PolicyVersionStore{db: db}
}

const policyVersionColumns = "id, policy_id, version, content_json, content_hash, signing_json, created_at"

func scanPolicyVersion(row pgx.Row) (PolicyVersion, error) {
	var v PolicyVersion
	err := row.Scan(&v.ID, &v.PolicyID, &v.Version, &v.ContentJSON, &v.ContentHash, &v.SigningJSON, &v.CreatedAt)
	return v, err
}

// CreatePolicyVersionParams carries the fields for [PolicyVersionStore.Create].
// Version must be > 0. A nil SigningJSON stores NULL.
type CreatePolicyVersionParams struct {
	PolicyID    string
	Version     int
	ContentJSON json.RawMessage
	ContentHash string
	SigningJSON json.RawMessage
}

// Create inserts a policy version and returns the stored row. A duplicate
// (policy_id, version) yields [ErrConflict]; the database also rejects
// version <= 0.
func (s *PolicyVersionStore) Create(ctx context.Context, p CreatePolicyVersionParams) (PolicyVersion, error) {
	const q = `INSERT INTO policy_versions (policy_id, version, content_json, content_hash, signing_json)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + policyVersionColumns
	version, err := scanPolicyVersion(s.db.QueryRow(ctx, q, p.PolicyID, p.Version, p.ContentJSON, p.ContentHash, p.SigningJSON))
	if err != nil {
		return PolicyVersion{}, fmt.Errorf("store: create policy version %d for policy %q: %w", p.Version, p.PolicyID, mapError(err))
	}
	return version, nil
}

// GetByPolicyAndVersion returns the given version of policyID, or [ErrNotFound].
func (s *PolicyVersionStore) GetByPolicyAndVersion(ctx context.Context, policyID string, version int) (PolicyVersion, error) {
	const q = `SELECT ` + policyVersionColumns + ` FROM policy_versions WHERE policy_id = $1 AND version = $2`
	v, err := scanPolicyVersion(s.db.QueryRow(ctx, q, policyID, version))
	if err != nil {
		return PolicyVersion{}, fmt.Errorf("store: policy version %d for policy %q: %w", version, policyID, mapError(err))
	}
	return v, nil
}

// ListByPolicy returns every version of policyID, ordered by version ascending.
func (s *PolicyVersionStore) ListByPolicy(ctx context.Context, policyID string) ([]PolicyVersion, error) {
	const q = `SELECT ` + policyVersionColumns + ` FROM policy_versions WHERE policy_id = $1 ORDER BY version`
	rows, err := s.db.Query(ctx, q, policyID)
	if err != nil {
		return nil, fmt.Errorf("store: list policy versions for policy %q: %w", policyID, mapError(err))
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PolicyVersion, error) {
		return scanPolicyVersion(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list policy versions for policy %q: %w", policyID, mapError(err))
	}
	return versions, nil
}
