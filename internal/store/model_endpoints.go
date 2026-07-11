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
