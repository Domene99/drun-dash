package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TeamMembershipStore reads and writes the team_memberships join table linking
// members to teams.
type TeamMembershipStore struct {
	db Querier
}

// NewTeamMembershipStore returns a TeamMembershipStore backed by db.
func NewTeamMembershipStore(db Querier) *TeamMembershipStore {
	return &TeamMembershipStore{db: db}
}

// Add links memberID to teamID and returns the stored row. Adding a member
// already on the team violates the (team_id, member_id) primary key and yields
// [ErrConflict].
func (s *TeamMembershipStore) Add(ctx context.Context, teamID, memberID string) (TeamMembership, error) {
	const q = `INSERT INTO team_memberships (team_id, member_id) VALUES ($1, $2)
		RETURNING team_id, member_id, added_at`
	var tm TeamMembership
	err := s.db.QueryRow(ctx, q, teamID, memberID).Scan(&tm.TeamID, &tm.MemberID, &tm.AddedAt)
	if err != nil {
		return TeamMembership{}, fmt.Errorf("store: add member %q to team %q: %w", memberID, teamID, mapError(err))
	}
	return tm, nil
}

// Remove unlinks memberID from teamID. It returns [ErrNotFound] if the member
// was not on the team.
func (s *TeamMembershipStore) Remove(ctx context.Context, teamID, memberID string) error {
	const q = `DELETE FROM team_memberships WHERE team_id = $1 AND member_id = $2`
	tag, err := s.db.Exec(ctx, q, teamID, memberID)
	if err != nil {
		return fmt.Errorf("store: remove member %q from team %q: %w", memberID, teamID, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: remove member %q from team %q: %w", memberID, teamID, ErrNotFound)
	}
	return nil
}

// ListMembers returns every member of teamID, ordered by email. Members not on
// the team are excluded; an empty result is not an error.
func (s *TeamMembershipStore) ListMembers(ctx context.Context, teamID string) ([]Member, error) {
	const q = `SELECT m.id, m.org_id, m.idp_subject, m.email, m.display_name, m.role, m.active, m.created_at, m.password_hash
		FROM members m
		JOIN team_memberships tm ON tm.member_id = m.id
		WHERE tm.team_id = $1
		ORDER BY m.email`
	rows, err := s.db.Query(ctx, q, teamID)
	if err != nil {
		return nil, fmt.Errorf("store: list members of team %q: %w", teamID, mapError(err))
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		return scanMember(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list members of team %q: %w", teamID, mapError(err))
	}
	return members, nil
}

// ListTeams returns every team memberID belongs to, ordered by name. An empty
// result is not an error.
func (s *TeamMembershipStore) ListTeams(ctx context.Context, memberID string) ([]Team, error) {
	const q = `SELECT t.id, t.org_id, t.name, t.created_at
		FROM teams t
		JOIN team_memberships tm ON tm.team_id = t.id
		WHERE tm.member_id = $1
		ORDER BY t.name`
	rows, err := s.db.Query(ctx, q, memberID)
	if err != nil {
		return nil, fmt.Errorf("store: list teams for member %q: %w", memberID, mapError(err))
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Team, error) {
		return scanTeam(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list teams for member %q: %w", memberID, mapError(err))
	}
	return teams, nil
}
