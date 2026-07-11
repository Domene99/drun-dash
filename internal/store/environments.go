package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EnvironmentStore reads and writes the environments table.
type EnvironmentStore struct {
	db Querier
}

// NewEnvironmentStore returns an EnvironmentStore backed by db.
func NewEnvironmentStore(db Querier) *EnvironmentStore {
	return &EnvironmentStore{db: db}
}

const envColumns = "id, org_id, name, created_at"

func scanEnvironment(row pgx.Row) (Environment, error) {
	var e Environment
	err := row.Scan(&e.ID, &e.OrgID, &e.Name, &e.CreatedAt)
	return e, err
}

// Create inserts an environment named name under orgID and returns the stored
// row. A duplicate (org_id, name) yields [ErrConflict].
func (s *EnvironmentStore) Create(ctx context.Context, orgID, name string) (Environment, error) {
	const q = `INSERT INTO environments (org_id, name) VALUES ($1, $2) RETURNING ` + envColumns
	env, err := scanEnvironment(s.db.QueryRow(ctx, q, orgID, name))
	if err != nil {
		return Environment{}, fmt.Errorf("store: create environment %q: %w", name, mapError(err))
	}
	return env, nil
}

// GetByID returns the environment with the given id, or [ErrNotFound].
func (s *EnvironmentStore) GetByID(ctx context.Context, id string) (Environment, error) {
	const q = `SELECT ` + envColumns + ` FROM environments WHERE id = $1`
	env, err := scanEnvironment(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Environment{}, fmt.Errorf("store: environment by id %q: %w", id, mapError(err))
	}
	return env, nil
}

// ListByOrg returns every environment belonging to orgID, ordered by name.
func (s *EnvironmentStore) ListByOrg(ctx context.Context, orgID string) ([]Environment, error) {
	const q = `SELECT ` + envColumns + ` FROM environments WHERE org_id = $1 ORDER BY name`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list environments for org %q: %w", orgID, mapError(err))
	}
	envs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Environment, error) {
		return scanEnvironment(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list environments for org %q: %w", orgID, mapError(err))
	}
	return envs, nil
}
