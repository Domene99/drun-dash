package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// UsageEventStore appends to and reads the usage_events table. The table is
// append-only: there is no update or delete method.
type UsageEventStore struct {
	db Querier
}

// NewUsageEventStore returns a UsageEventStore backed by db.
func NewUsageEventStore(db Querier) *UsageEventStore {
	return &UsageEventStore{db: db}
}

// usageEventColumns lists every column, including the DB-generated total_tokens,
// in the order scanUsageEvent expects. total_tokens is read-only, so it never
// appears in an INSERT column list.
const usageEventColumns = "id, org_id, session_id, agent_id, endpoint_id, model, prompt_tokens, completion_tokens, total_tokens, cost_amount, currency, occurred_at"

func scanUsageEvent(row pgx.Row) (UsageEvent, error) {
	var e UsageEvent
	err := row.Scan(&e.ID, &e.OrgID, &e.SessionID, &e.AgentID, &e.EndpointID, &e.Model,
		&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.CostAmount, &e.Currency, &e.OccurredAt)
	return e, err
}

// AppendUsageEventParams carries the fields for [UsageEventStore.Append]. The
// four attribution ids and Model are optional (nil stores NULL). PromptTokens
// and CompletionTokens default to 0; total_tokens is DB-generated and must not
// be supplied. An invalid CostAmount (Valid == false) stores 0, matching the
// column default. An empty Currency defaults to 'USD'.
type AppendUsageEventParams struct {
	OrgID            string
	SessionID        *string
	AgentID          *string
	EndpointID       *string
	Model            *string
	PromptTokens     int
	CompletionTokens int
	CostAmount       pgtype.Numeric
	Currency         string
}

// Append inserts a usage event and returns the stored row, including the
// database-generated id, total_tokens, and occurred_at. usage_events is
// append-only, so this is the only write path.
func (s *UsageEventStore) Append(ctx context.Context, p AppendUsageEventParams) (UsageEvent, error) {
	if p.Currency == "" {
		p.Currency = "USD"
	}
	const q = `INSERT INTO usage_events
		(org_id, session_id, agent_id, endpoint_id, model, prompt_tokens, completion_tokens, cost_amount, currency)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8::numeric, 0), $9) RETURNING ` + usageEventColumns
	event, err := scanUsageEvent(s.db.QueryRow(ctx, q,
		p.OrgID, p.SessionID, p.AgentID, p.EndpointID, p.Model,
		p.PromptTokens, p.CompletionTokens, p.CostAmount, p.Currency))
	if err != nil {
		return UsageEvent{}, fmt.Errorf("store: append usage event for org %q: %w", p.OrgID, mapError(err))
	}
	return event, nil
}

// ListByOrg returns every usage event for orgID, ordered by occurred_at then id
// for a stable chronological order.
func (s *UsageEventStore) ListByOrg(ctx context.Context, orgID string) ([]UsageEvent, error) {
	const q = `SELECT ` + usageEventColumns + ` FROM usage_events WHERE org_id = $1 ORDER BY occurred_at, id`
	return s.list(ctx, q, orgID, fmt.Sprintf("org %q", orgID))
}

// ListBySession returns every usage event attributed to sessionID, ordered by
// occurred_at then id for a stable chronological order.
func (s *UsageEventStore) ListBySession(ctx context.Context, sessionID string) ([]UsageEvent, error) {
	const q = `SELECT ` + usageEventColumns + ` FROM usage_events WHERE session_id = $1 ORDER BY occurred_at, id`
	return s.list(ctx, q, sessionID, fmt.Sprintf("session %q", sessionID))
}

// list runs a single-argument usage-event query and collects the rows. subject
// is a human-readable description of the filter for error context.
func (s *UsageEventStore) list(ctx context.Context, q, arg, subject string) ([]UsageEvent, error) {
	rows, err := s.db.Query(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list usage events for %s: %w", subject, mapError(err))
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageEvent, error) {
		return scanUsageEvent(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list usage events for %s: %w", subject, mapError(err))
	}
	return events, nil
}
