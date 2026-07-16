package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ModelEndpointStore reads and writes the model_endpoints table.
type ModelEndpointStore struct {
	db Querier
}

// NewModelEndpointStore returns a ModelEndpointStore backed by db.
func NewModelEndpointStore(db Querier) *ModelEndpointStore {
	return &ModelEndpointStore{db: db}
}

const modelEndpointColumns = "id, org_id, name, base_url, provider, secret_ref, active, created_at"

func scanModelEndpoint(row pgx.Row) (ModelEndpoint, error) {
	var e ModelEndpoint
	err := row.Scan(&e.ID, &e.OrgID, &e.Name, &e.BaseURL, &e.Provider, &e.SecretRef, &e.Active, &e.CreatedAt)
	return e, err
}

// CreateModelEndpointParams carries the fields for [ModelEndpointStore.Create].
// The database assigns id, active (true), and created_at.
type CreateModelEndpointParams struct {
	OrgID     string
	Name      string
	BaseURL   string
	Provider  string
	SecretRef string
}

// Create inserts a model endpoint and returns the stored row. A duplicate
// (org_id, name) yields [ErrConflict].
func (s *ModelEndpointStore) Create(ctx context.Context, p CreateModelEndpointParams) (ModelEndpoint, error) {
	const q = `INSERT INTO model_endpoints (org_id, name, base_url, provider, secret_ref)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + modelEndpointColumns
	ep, err := scanModelEndpoint(s.db.QueryRow(ctx, q, p.OrgID, p.Name, p.BaseURL, p.Provider, p.SecretRef))
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("store: create model endpoint %q: %w", p.Name, mapError(err))
	}
	return ep, nil
}

// GetByID returns the model endpoint with the given id, or [ErrNotFound].
func (s *ModelEndpointStore) GetByID(ctx context.Context, id string) (ModelEndpoint, error) {
	const q = `SELECT ` + modelEndpointColumns + ` FROM model_endpoints WHERE id = $1`
	ep, err := scanModelEndpoint(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("store: model endpoint by id %q: %w", id, mapError(err))
	}
	return ep, nil
}

// ListByOrg returns every model endpoint belonging to orgID, ordered by name.
func (s *ModelEndpointStore) ListByOrg(ctx context.Context, orgID string) ([]ModelEndpoint, error) {
	const q = `SELECT ` + modelEndpointColumns + ` FROM model_endpoints WHERE org_id = $1 ORDER BY name`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list model endpoints for org %q: %w", orgID, mapError(err))
	}
	eps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ModelEndpoint, error) {
		return scanModelEndpoint(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list model endpoints for org %q: %w", orgID, mapError(err))
	}
	return eps, nil
}

// UpdateModelEndpointParams carries the mutable fields for
// [ModelEndpointStore.Update]: the endpoint's identity/routing metadata and its
// opaque secret_ref. The active flag is toggled separately via
// [ModelEndpointStore.SetActive].
type UpdateModelEndpointParams struct {
	ID        string
	Name      string
	BaseURL   string
	Provider  string
	SecretRef string
}

// Update replaces the name, base_url, provider, and secret_ref of an existing
// endpoint and returns the updated row. A rename colliding with another
// (org_id, name) yields [ErrConflict]; a missing id yields [ErrNotFound].
func (s *ModelEndpointStore) Update(ctx context.Context, p UpdateModelEndpointParams) (ModelEndpoint, error) {
	const q = `UPDATE model_endpoints SET name = $2, base_url = $3, provider = $4, secret_ref = $5
		WHERE id = $1 RETURNING ` + modelEndpointColumns
	ep, err := scanModelEndpoint(s.db.QueryRow(ctx, q, p.ID, p.Name, p.BaseURL, p.Provider, p.SecretRef))
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("store: update model endpoint %q: %w", p.ID, mapError(err))
	}
	return ep, nil
}

// Delete removes the endpoint with the given id. It returns [ErrNotFound] if no
// such endpoint exists. The backing sealed secret (if any) is not removed here:
// the secret_ref is opaque to this store, so the caller deletes the secret
// through the vault.
func (s *ModelEndpointStore) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM model_endpoints WHERE id = $1`
	tag, err := s.db.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: delete model endpoint %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete model endpoint %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetActive flips the active flag on the endpoint with the given id and returns
// the updated row. It returns [ErrNotFound] if no such endpoint exists.
func (s *ModelEndpointStore) SetActive(ctx context.Context, id string, active bool) (ModelEndpoint, error) {
	const q = `UPDATE model_endpoints SET active = $2 WHERE id = $1 RETURNING ` + modelEndpointColumns
	ep, err := scanModelEndpoint(s.db.QueryRow(ctx, q, id, active))
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("store: set model endpoint %q active=%t: %w", id, active, mapError(err))
	}
	return ep, nil
}
