package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AgentVersionStore reads and writes the agent_versions table.
type AgentVersionStore struct {
	db Querier
}

// NewAgentVersionStore returns an AgentVersionStore backed by db.
func NewAgentVersionStore(db Querier) *AgentVersionStore {
	return &AgentVersionStore{db: db}
}

const agentVersionColumns = "id, agent_id, version, system_prompt, allowed_tools, policy_version_id, endpoint_id, budget_id, fan_out_ceiling, created_at"

func scanAgentVersion(row pgx.Row) (AgentVersion, error) {
	var v AgentVersion
	err := row.Scan(&v.ID, &v.AgentID, &v.Version, &v.SystemPrompt, &v.AllowedTools,
		&v.PolicyVersionID, &v.EndpointID, &v.BudgetID, &v.FanOutCeiling, &v.CreatedAt)
	return v, err
}

// CreateAgentVersionParams carries the fields for [AgentVersionStore.Create].
// Version must be > 0. A nil AllowedTools stores the column default ('[]'). The
// three id pins are optional (nil stores NULL). FanOutCeiling is stored as
// given; pass 1 to match the column default.
type CreateAgentVersionParams struct {
	AgentID         string
	Version         int
	SystemPrompt    string
	AllowedTools    json.RawMessage
	PolicyVersionID *string
	EndpointID      *string
	BudgetID        *string
	FanOutCeiling   int
}

// Create inserts an agent version and returns the stored row. A duplicate
// (agent_id, version) yields [ErrConflict]; the database also rejects
// version <= 0. A nil pin (policy version, endpoint, budget) stores NULL; a
// non-nil pin that references a missing row is rejected by the foreign key.
func (s *AgentVersionStore) Create(ctx context.Context, p CreateAgentVersionParams) (AgentVersion, error) {
	const q = `INSERT INTO agent_versions
		(agent_id, version, system_prompt, allowed_tools, policy_version_id, endpoint_id, budget_id, fan_out_ceiling)
		VALUES ($1, $2, $3, COALESCE($4, '[]'::jsonb), $5, $6, $7, $8) RETURNING ` + agentVersionColumns
	version, err := scanAgentVersion(s.db.QueryRow(ctx, q,
		p.AgentID, p.Version, p.SystemPrompt, p.AllowedTools, p.PolicyVersionID, p.EndpointID, p.BudgetID, p.FanOutCeiling))
	if err != nil {
		return AgentVersion{}, fmt.Errorf("store: create agent version %d for agent %q: %w", p.Version, p.AgentID, mapError(err))
	}
	return version, nil
}

// GetByAgentAndVersion returns the given version of agentID, or [ErrNotFound].
func (s *AgentVersionStore) GetByAgentAndVersion(ctx context.Context, agentID string, version int) (AgentVersion, error) {
	const q = `SELECT ` + agentVersionColumns + ` FROM agent_versions WHERE agent_id = $1 AND version = $2`
	v, err := scanAgentVersion(s.db.QueryRow(ctx, q, agentID, version))
	if err != nil {
		return AgentVersion{}, fmt.Errorf("store: agent version %d for agent %q: %w", version, agentID, mapError(err))
	}
	return v, nil
}

// ListByAgent returns every version of agentID, ordered by version ascending.
func (s *AgentVersionStore) ListByAgent(ctx context.Context, agentID string) ([]AgentVersion, error) {
	const q = `SELECT ` + agentVersionColumns + ` FROM agent_versions WHERE agent_id = $1 ORDER BY version`
	rows, err := s.db.Query(ctx, q, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list agent versions for agent %q: %w", agentID, mapError(err))
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AgentVersion, error) {
		return scanAgentVersion(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list agent versions for agent %q: %w", agentID, mapError(err))
	}
	return versions, nil
}
