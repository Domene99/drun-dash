package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EntitlementStore reads and writes the entitlements table.
type EntitlementStore struct {
	db Querier
}

// NewEntitlementStore returns an EntitlementStore backed by db.
func NewEntitlementStore(db Querier) *EntitlementStore {
	return &EntitlementStore{db: db}
}

const entitlementColumns = "id, agent_id, member_id, team_id, policy_version_id, max_fan_out, created_at"

func scanEntitlement(row pgx.Row) (Entitlement, error) {
	var e Entitlement
	err := row.Scan(&e.ID, &e.AgentID, &e.MemberID, &e.TeamID, &e.PolicyVersionID, &e.MaxFanOut, &e.CreatedAt)
	return e, err
}

// CreateEntitlementParams carries the fields for [EntitlementStore.Create].
// Exactly one of MemberID / TeamID must be set (a member XOR a team).
// PolicyVersionID and MaxFanOut are optional grant overrides.
type CreateEntitlementParams struct {
	AgentID         string
	MemberID        *string
	TeamID          *string
	PolicyVersionID *string
	MaxFanOut       *int
}

// Create inserts an entitlement and returns the stored row. Exactly one of
// MemberID / TeamID must be non-nil; this is validated in Go before the insert
// and returns a descriptive error (not [ErrConflict]) when violated, mirroring
// the database's member-XOR-team CHECK constraint. A grant that duplicates an
// existing (agent, principal) pair yields [ErrConflict].
func (s *EntitlementStore) Create(ctx context.Context, p CreateEntitlementParams) (Entitlement, error) {
	switch {
	case p.MemberID == nil && p.TeamID == nil:
		return Entitlement{}, fmt.Errorf("store: create entitlement for agent %q: exactly one of member_id/team_id must be set, got neither", p.AgentID)
	case p.MemberID != nil && p.TeamID != nil:
		return Entitlement{}, fmt.Errorf("store: create entitlement for agent %q: exactly one of member_id/team_id must be set, got both", p.AgentID)
	}
	const q = `INSERT INTO entitlements (agent_id, member_id, team_id, policy_version_id, max_fan_out)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + entitlementColumns
	ent, err := scanEntitlement(s.db.QueryRow(ctx, q, p.AgentID, p.MemberID, p.TeamID, p.PolicyVersionID, p.MaxFanOut))
	if err != nil {
		return Entitlement{}, fmt.Errorf("store: create entitlement for agent %q: %w", p.AgentID, mapError(err))
	}
	return ent, nil
}

// ListByAgent returns every entitlement granting agentID, ordered by created_at
// then id for a stable order.
func (s *EntitlementStore) ListByAgent(ctx context.Context, agentID string) ([]Entitlement, error) {
	const q = `SELECT ` + entitlementColumns + ` FROM entitlements WHERE agent_id = $1 ORDER BY created_at, id`
	return s.list(ctx, q, agentID, fmt.Sprintf("agent %q", agentID))
}

// ListByMember returns every entitlement granted to memberID, ordered by
// created_at then id for a stable order.
func (s *EntitlementStore) ListByMember(ctx context.Context, memberID string) ([]Entitlement, error) {
	const q = `SELECT ` + entitlementColumns + ` FROM entitlements WHERE member_id = $1 ORDER BY created_at, id`
	return s.list(ctx, q, memberID, fmt.Sprintf("member %q", memberID))
}

// ListByTeam returns every entitlement granted to teamID, ordered by created_at
// then id for a stable order.
func (s *EntitlementStore) ListByTeam(ctx context.Context, teamID string) ([]Entitlement, error) {
	const q = `SELECT ` + entitlementColumns + ` FROM entitlements WHERE team_id = $1 ORDER BY created_at, id`
	return s.list(ctx, q, teamID, fmt.Sprintf("team %q", teamID))
}

// list runs a single-argument entitlement query and collects the rows. subject
// is a human-readable description of the filter for error context.
func (s *EntitlementStore) list(ctx context.Context, q, arg, subject string) ([]Entitlement, error) {
	rows, err := s.db.Query(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list entitlements for %s: %w", subject, mapError(err))
	}
	ents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entitlement, error) {
		return scanEntitlement(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list entitlements for %s: %w", subject, mapError(err))
	}
	return ents, nil
}

// Delete removes the entitlement with the given id. It returns [ErrNotFound] if
// no such entitlement exists.
func (s *EntitlementStore) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM entitlements WHERE id = $1`
	tag, err := s.db.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: delete entitlement %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete entitlement %q: %w", id, ErrNotFound)
	}
	return nil
}
