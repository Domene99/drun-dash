package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AgentStore reads and writes the agents table.
type AgentStore struct {
	db Querier
}

// NewAgentStore returns an AgentStore backed by db.
func NewAgentStore(db Querier) *AgentStore {
	return &AgentStore{db: db}
}

const agentColumns = "id, org_id, name, description, current_version, created_at, updated_at"

func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	err := row.Scan(&a.ID, &a.OrgID, &a.Name, &a.Description, &a.CurrentVersion, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// Create inserts an agent named name under orgID and returns the stored row,
// with current_version starting at 0. A nil description stores NULL. A
// duplicate (org_id, name) yields [ErrConflict].
func (s *AgentStore) Create(ctx context.Context, orgID, name string, description *string) (Agent, error) {
	const q = `INSERT INTO agents (org_id, name, description) VALUES ($1, $2, $3) RETURNING ` + agentColumns
	agent, err := scanAgent(s.db.QueryRow(ctx, q, orgID, name, description))
	if err != nil {
		return Agent{}, fmt.Errorf("store: create agent %q: %w", name, mapError(err))
	}
	return agent, nil
}

// GetByID returns the agent with the given id, or [ErrNotFound].
func (s *AgentStore) GetByID(ctx context.Context, id string) (Agent, error) {
	const q = `SELECT ` + agentColumns + ` FROM agents WHERE id = $1`
	agent, err := scanAgent(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Agent{}, fmt.Errorf("store: agent by id %q: %w", id, mapError(err))
	}
	return agent, nil
}

// ListByOrg returns every agent belonging to orgID, ordered by name.
func (s *AgentStore) ListByOrg(ctx context.Context, orgID string) ([]Agent, error) {
	const q = `SELECT ` + agentColumns + ` FROM agents WHERE org_id = $1 ORDER BY name`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list agents for org %q: %w", orgID, mapError(err))
	}
	agents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Agent, error) {
		return scanAgent(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list agents for org %q: %w", orgID, mapError(err))
	}
	return agents, nil
}

// ListByOrgPage returns up to limit agents for orgID ordered by (created_at,
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
func (s *AgentStore) ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]Agent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list agents page for org %q: limit must be positive, got %d", orgID, limit)
	}
	q := `SELECT ` + agentColumns + ` FROM agents WHERE org_id = $1`
	args := []any{orgID, limit}
	if !afterCreatedAt.IsZero() {
		q += ` AND (created_at, id) > ($3, $4)`
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY created_at, id LIMIT $2`
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list agents page for org %q: %w", orgID, mapError(err))
	}
	agents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Agent, error) {
		return scanAgent(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list agents page for org %q: %w", orgID, mapError(err))
	}
	return agents, nil
}

// SetCurrentVersion updates the current_version pointer on the agent with the
// given id, bumps updated_at, and returns the updated row. It returns
// [ErrNotFound] if no such agent exists. Callers are responsible for having
// created the referenced [AgentVersion] first.
func (s *AgentStore) SetCurrentVersion(ctx context.Context, id string, version int) (Agent, error) {
	const q = `UPDATE agents SET current_version = $2, updated_at = now() WHERE id = $1 RETURNING ` + agentColumns
	agent, err := scanAgent(s.db.QueryRow(ctx, q, id, version))
	if err != nil {
		return Agent{}, fmt.Errorf("store: set agent %q current version=%d: %w", id, version, mapError(err))
	}
	return agent, nil
}
