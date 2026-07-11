package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SessionStore reads and writes the sessions table.
type SessionStore struct {
	db Querier
}

// NewSessionStore returns a SessionStore backed by db.
func NewSessionStore(db Querier) *SessionStore {
	return &SessionStore{db: db}
}

const sessionColumns = "id, org_id, agent_id, agent_version, policy_version_id, environment_id, launched_by, status, fan_out_ceiling, labels, created_at, terminated_at"

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.OrgID, &s.AgentID, &s.AgentVersion, &s.PolicyVersionID,
		&s.EnvironmentID, &s.LaunchedBy, &s.Status, &s.FanOutCeiling, &s.Labels,
		&s.CreatedAt, &s.TerminatedAt)
	return s, err
}

// CreateSessionParams carries the fields for [SessionStore.Create]. An empty
// Status stores the column default ('pending'); a nil Labels stores the default
// ('{}'). The four nullable ids (agent version, policy version, environment,
// launched-by member) store NULL when nil. FanOutCeiling is stored as given;
// pass 1 to match the column default.
type CreateSessionParams struct {
	OrgID           string
	AgentID         string
	AgentVersion    *int
	PolicyVersionID *string
	EnvironmentID   *string
	LaunchedBy      *string
	Status          SessionStatus
	FanOutCeiling   int
	Labels          json.RawMessage
}

// Create inserts a session and returns the stored row. An empty Status defaults
// to pending. The database enforces the session_status enum domain and the
// agents foreign key (ON DELETE RESTRICT); a missing agent (or any other
// missing referenced row) is rejected by the foreign key.
func (s *SessionStore) Create(ctx context.Context, p CreateSessionParams) (Session, error) {
	if p.Status == "" {
		p.Status = SessionStatusPending
	}
	const q = `INSERT INTO sessions
		(org_id, agent_id, agent_version, policy_version_id, environment_id, launched_by, status, fan_out_ceiling, labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, '{}'::jsonb)) RETURNING ` + sessionColumns
	session, err := scanSession(s.db.QueryRow(ctx, q,
		p.OrgID, p.AgentID, p.AgentVersion, p.PolicyVersionID, p.EnvironmentID,
		p.LaunchedBy, p.Status, p.FanOutCeiling, p.Labels))
	if err != nil {
		return Session{}, fmt.Errorf("store: create session for agent %q: %w", p.AgentID, mapError(err))
	}
	return session, nil
}

// GetByID returns the session with the given id, or [ErrNotFound].
func (s *SessionStore) GetByID(ctx context.Context, id string) (Session, error) {
	const q = `SELECT ` + sessionColumns + ` FROM sessions WHERE id = $1`
	session, err := scanSession(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Session{}, fmt.Errorf("store: session by id %q: %w", id, mapError(err))
	}
	return session, nil
}

// ListByOrg returns every session belonging to orgID, newest first (by
// created_at descending, then id for a stable order).
func (s *SessionStore) ListByOrg(ctx context.Context, orgID string) ([]Session, error) {
	const q = `SELECT ` + sessionColumns + ` FROM sessions WHERE org_id = $1 ORDER BY created_at DESC, id`
	return s.list(ctx, q, orgID, fmt.Sprintf("org %q", orgID))
}

// ListByOrgPage returns up to limit sessions for orgID ordered by (created_at,
// id) ascending, starting strictly after the (afterCreatedAt, afterID)
// position. A zero afterCreatedAt (afterID ignored) starts from the beginning
// of the list. The caller typically passes limit+1 to detect whether another
// page exists and to derive the next cursor from the last kept row; this method
// only runs the seek and returns the rows. It is the keyset-paginated
// counterpart to [ListByOrg] (which orders newest-first and is left unchanged)
// per ADR-0002 §3.
//
// A non-positive limit is rejected as a programming error: the handler layer is
// the primary bound (parsing and clamping the query's limit to [1, 200]), so a
// limit of zero or below reaching the store is a bug, not a request for an
// empty page.
func (s *SessionStore) ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]Session, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list sessions page for org %q: limit must be positive, got %d", orgID, limit)
	}
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE org_id = $1`
	args := []any{orgID, limit}
	if !afterCreatedAt.IsZero() {
		q += ` AND (created_at, id) > ($3, $4)`
		args = append(args, afterCreatedAt, afterID)
	}
	q += ` ORDER BY created_at, id LIMIT $2`
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions page for org %q: %w", orgID, mapError(err))
	}
	sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
		return scanSession(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list sessions page for org %q: %w", orgID, mapError(err))
	}
	return sessions, nil
}

// ListByAgent returns every session that ran agentID, newest first (by
// created_at descending, then id for a stable order).
func (s *SessionStore) ListByAgent(ctx context.Context, agentID string) ([]Session, error) {
	const q = `SELECT ` + sessionColumns + ` FROM sessions WHERE agent_id = $1 ORDER BY created_at DESC, id`
	return s.list(ctx, q, agentID, fmt.Sprintf("agent %q", agentID))
}

// list runs a single-argument session query and collects the rows. subject is a
// human-readable description of the filter for error context.
func (s *SessionStore) list(ctx context.Context, q, arg, subject string) ([]Session, error) {
	rows, err := s.db.Query(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions for %s: %w", subject, mapError(err))
	}
	sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
		return scanSession(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list sessions for %s: %w", subject, mapError(err))
	}
	return sessions, nil
}

// SetStatus updates the status of the session with the given id and returns the
// updated row. Moving to a terminal status (terminated or failed) stamps
// terminated_at with now() if it is not already set; other transitions leave it
// untouched. It returns [ErrNotFound] if no such session exists.
func (s *SessionStore) SetStatus(ctx context.Context, id string, status SessionStatus) (Session, error) {
	// terminal is passed as its own boolean parameter rather than re-deriving it
	// from $2 in SQL: referencing $2 both as a session_status (status = $2) and
	// inside an IN (...) of text literals makes Postgres deduce inconsistent
	// types for the parameter (SQLSTATE 42P08).
	terminal := status == SessionStatusTerminated || status == SessionStatusFailed
	const q = `UPDATE sessions
		SET status = $2,
		    terminated_at = CASE
		        WHEN $3 AND terminated_at IS NULL THEN now()
		        ELSE terminated_at
		    END
		WHERE id = $1 RETURNING ` + sessionColumns
	session, err := scanSession(s.db.QueryRow(ctx, q, id, status, terminal))
	if err != nil {
		return Session{}, fmt.Errorf("store: set session %q status=%q: %w", id, status, mapError(err))
	}
	return session, nil
}
