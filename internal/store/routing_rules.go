package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RoutingRuleStore reads and writes the routing_rules table.
type RoutingRuleStore struct {
	db Querier
}

// NewRoutingRuleStore returns a RoutingRuleStore backed by db.
func NewRoutingRuleStore(db Querier) *RoutingRuleStore {
	return &RoutingRuleStore{db: db}
}

const routingRuleColumns = "id, org_id, priority, match_json, endpoint_id, created_at"

func scanRoutingRule(row pgx.Row) (RoutingRule, error) {
	var r RoutingRule
	err := row.Scan(&r.ID, &r.OrgID, &r.Priority, &r.MatchJSON, &r.EndpointID, &r.CreatedAt)
	return r, err
}

// CreateRoutingRuleParams carries the fields for [RoutingRuleStore.Create]. A
// nil MatchJSON stores the column default ('{}').
type CreateRoutingRuleParams struct {
	OrgID      string
	Priority   int
	MatchJSON  json.RawMessage
	EndpointID string
}

// Create inserts a routing rule and returns the stored row. A nil MatchJSON
// falls back to the column default. Referencing an endpoint in another org (or
// none) is rejected by the foreign key.
func (s *RoutingRuleStore) Create(ctx context.Context, p CreateRoutingRuleParams) (RoutingRule, error) {
	const q = `INSERT INTO routing_rules (org_id, priority, match_json, endpoint_id)
		VALUES ($1, $2, COALESCE($3, '{}'::jsonb), $4) RETURNING ` + routingRuleColumns
	rule, err := scanRoutingRule(s.db.QueryRow(ctx, q, p.OrgID, p.Priority, p.MatchJSON, p.EndpointID))
	if err != nil {
		return RoutingRule{}, fmt.Errorf("store: create routing rule for org %q: %w", p.OrgID, mapError(err))
	}
	return rule, nil
}

// ListByOrg returns every routing rule belonging to orgID, ordered by priority
// (ascending, lowest evaluated first) then created_at for a stable order.
func (s *RoutingRuleStore) ListByOrg(ctx context.Context, orgID string) ([]RoutingRule, error) {
	const q = `SELECT ` + routingRuleColumns + ` FROM routing_rules WHERE org_id = $1 ORDER BY priority, created_at, id`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list routing rules for org %q: %w", orgID, mapError(err))
	}
	rules, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RoutingRule, error) {
		return scanRoutingRule(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list routing rules for org %q: %w", orgID, mapError(err))
	}
	return rules, nil
}

// Delete removes the routing rule with the given id. It returns [ErrNotFound]
// if no such rule exists.
func (s *RoutingRuleStore) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM routing_rules WHERE id = $1`
	tag, err := s.db.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: delete routing rule %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete routing rule %q: %w", id, ErrNotFound)
	}
	return nil
}
