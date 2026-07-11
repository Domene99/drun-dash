package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TeamStore reads and writes the teams table.
type TeamStore struct {
	db Querier
}

// NewTeamStore returns a TeamStore backed by db.
func NewTeamStore(db Querier) *TeamStore {
	return &TeamStore{db: db}
}

const teamColumns = "id, org_id, name, created_at"

func scanTeam(row pgx.Row) (Team, error) {
	var t Team
	err := row.Scan(&t.ID, &t.OrgID, &t.Name, &t.CreatedAt)
	return t, err
}

// Create inserts a team named name under orgID and returns the stored row. A
// duplicate (org_id, name) yields [ErrConflict].
func (s *TeamStore) Create(ctx context.Context, orgID, name string) (Team, error) {
	const q = `INSERT INTO teams (org_id, name) VALUES ($1, $2) RETURNING ` + teamColumns
	team, err := scanTeam(s.db.QueryRow(ctx, q, orgID, name))
	if err != nil {
		return Team{}, fmt.Errorf("store: create team %q: %w", name, mapError(err))
	}
	return team, nil
}

// GetByID returns the team with the given id, or [ErrNotFound].
func (s *TeamStore) GetByID(ctx context.Context, id string) (Team, error) {
	const q = `SELECT ` + teamColumns + ` FROM teams WHERE id = $1`
	team, err := scanTeam(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Team{}, fmt.Errorf("store: team by id %q: %w", id, mapError(err))
	}
	return team, nil
}

// ListByOrg returns every team belonging to orgID, ordered by name.
func (s *TeamStore) ListByOrg(ctx context.Context, orgID string) ([]Team, error) {
	const q = `SELECT ` + teamColumns + ` FROM teams WHERE org_id = $1 ORDER BY name`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list teams for org %q: %w", orgID, mapError(err))
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Team, error) {
		return scanTeam(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list teams for org %q: %w", orgID, mapError(err))
	}
	return teams, nil
}
