package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TeamRoleStore reads and writes the team_roles join table, which assigns roles
// to teams; every member of the team inherits the role (ADR-0006 §4).
type TeamRoleStore struct {
	db Querier
}

// NewTeamRoleStore returns a TeamRoleStore backed by db.
func NewTeamRoleStore(db Querier) *TeamRoleStore {
	return &TeamRoleStore{db: db}
}

// AssignRole assigns roleID to teamID and returns the stored row. Assigning a
// role the team already holds violates the (team_id, role_id) primary key and
// yields [ErrConflict].
func (s *TeamRoleStore) AssignRole(ctx context.Context, teamID, roleID string) (TeamRoleAssignment, error) {
	const q = `INSERT INTO team_roles (team_id, role_id) VALUES ($1, $2)
		RETURNING team_id, role_id`
	var tr TeamRoleAssignment
	if err := s.db.QueryRow(ctx, q, teamID, roleID).Scan(&tr.TeamID, &tr.RoleID); err != nil {
		return TeamRoleAssignment{}, fmt.Errorf("store: assign role %q to team %q: %w", roleID, teamID, mapError(err))
	}
	return tr, nil
}

// RemoveRole unassigns roleID from teamID. It returns [ErrNotFound] if the team
// did not hold the role, and [ErrLastOwner] if the role is the org's built-in
// Owner and removing it would leave the org with no Owner assignment
// (ADR-0006 §7).
func (s *TeamRoleStore) RemoveRole(ctx context.Context, teamID, roleID string) error {
	if err := guardLastOwner(ctx, s.db, roleID); err != nil {
		return fmt.Errorf("store: remove role %q from team %q: %w", roleID, teamID, err)
	}
	const q = `DELETE FROM team_roles WHERE team_id = $1 AND role_id = $2`
	tag, err := s.db.Exec(ctx, q, teamID, roleID)
	if err != nil {
		return fmt.Errorf("store: remove role %q from team %q: %w", roleID, teamID, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: remove role %q from team %q: %w", roleID, teamID, ErrNotFound)
	}
	return nil
}

// ListRolesForTeam returns the roles assigned to teamID, ordered by name, with
// permissions and agent access hydrated. An empty result is not an error.
func (s *TeamRoleStore) ListRolesForTeam(ctx context.Context, teamID string) ([]Role, error) {
	const q = `SELECT ` + prefixedRoleColumns + `
		FROM roles r
		JOIN team_roles tr ON tr.role_id = r.id
		WHERE tr.team_id = $1
		ORDER BY r.name`
	return hydrateRoleList(ctx, s.db, q, teamID, fmt.Sprintf("team %q", teamID))
}

// ListTeamsForRole returns the teams assigned roleID, ordered by name. An empty
// result is not an error.
func (s *TeamRoleStore) ListTeamsForRole(ctx context.Context, roleID string) ([]Team, error) {
	const q = `SELECT t.id, t.org_id, t.name, t.created_at
		FROM teams t
		JOIN team_roles tr ON tr.team_id = t.id
		WHERE tr.role_id = $1
		ORDER BY t.name`
	rows, err := s.db.Query(ctx, q, roleID)
	if err != nil {
		return nil, fmt.Errorf("store: list teams for role %q: %w", roleID, mapError(err))
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Team, error) {
		return scanTeam(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list teams for role %q: %w", roleID, mapError(err))
	}
	return teams, nil
}
