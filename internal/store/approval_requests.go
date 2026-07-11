package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ApprovalRequestStore reads and writes the approval_requests table.
type ApprovalRequestStore struct {
	db Querier
}

// NewApprovalRequestStore returns an ApprovalRequestStore backed by db.
func NewApprovalRequestStore(db Querier) *ApprovalRequestStore {
	return &ApprovalRequestStore{db: db}
}

const approvalRequestColumns = "id, org_id, session_id, action, reason, context_json, status, decided_by, decided_at, created_at"

func scanApprovalRequest(row pgx.Row) (ApprovalRequest, error) {
	var a ApprovalRequest
	err := row.Scan(&a.ID, &a.OrgID, &a.SessionID, &a.Action, &a.Reason, &a.ContextJSON,
		&a.Status, &a.DecidedBy, &a.DecidedAt, &a.CreatedAt)
	return a, err
}

// CreateApprovalRequestParams carries the fields for
// [ApprovalRequestStore.Create]. A new request always starts pending. A nil
// Reason stores NULL; a nil ContextJSON stores the column default ('{}').
type CreateApprovalRequestParams struct {
	OrgID       string
	SessionID   string
	Action      string
	Reason      *string
	ContextJSON json.RawMessage
}

// Create inserts a pending approval request and returns the stored row. A
// missing session (or org) is rejected by the foreign key.
func (s *ApprovalRequestStore) Create(ctx context.Context, p CreateApprovalRequestParams) (ApprovalRequest, error) {
	const q = `INSERT INTO approval_requests (org_id, session_id, action, reason, context_json)
		VALUES ($1, $2, $3, $4, COALESCE($5, '{}'::jsonb)) RETURNING ` + approvalRequestColumns
	req, err := scanApprovalRequest(s.db.QueryRow(ctx, q,
		p.OrgID, p.SessionID, p.Action, p.Reason, p.ContextJSON))
	if err != nil {
		return ApprovalRequest{}, fmt.Errorf("store: create approval request for session %q: %w", p.SessionID, mapError(err))
	}
	return req, nil
}

// GetByID returns the approval request with the given id, or [ErrNotFound].
func (s *ApprovalRequestStore) GetByID(ctx context.Context, id string) (ApprovalRequest, error) {
	const q = `SELECT ` + approvalRequestColumns + ` FROM approval_requests WHERE id = $1`
	req, err := scanApprovalRequest(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return ApprovalRequest{}, fmt.Errorf("store: approval request by id %q: %w", id, mapError(err))
	}
	return req, nil
}

// ListByOrg returns every approval request for orgID, oldest first (by
// created_at then id for a stable order).
func (s *ApprovalRequestStore) ListByOrg(ctx context.Context, orgID string) ([]ApprovalRequest, error) {
	const q = `SELECT ` + approvalRequestColumns + ` FROM approval_requests WHERE org_id = $1 ORDER BY created_at, id`
	return s.list(ctx, q, orgID, fmt.Sprintf("org %q", orgID))
}

// ListPendingByOrg returns the pending approval queue for orgID, oldest first —
// the "awaiting a decision" view.
func (s *ApprovalRequestStore) ListPendingByOrg(ctx context.Context, orgID string) ([]ApprovalRequest, error) {
	const q = `SELECT ` + approvalRequestColumns + ` FROM approval_requests WHERE org_id = $1 AND status = 'pending' ORDER BY created_at, id`
	return s.list(ctx, q, orgID, fmt.Sprintf("org %q pending", orgID))
}

// list runs a single-argument approval-request query and collects the rows.
// subject is a human-readable description of the filter for error context.
func (s *ApprovalRequestStore) list(ctx context.Context, q, arg, subject string) ([]ApprovalRequest, error) {
	rows, err := s.db.Query(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list approval requests for %s: %w", subject, mapError(err))
	}
	reqs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ApprovalRequest, error) {
		return scanApprovalRequest(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list approval requests for %s: %w", subject, mapError(err))
	}
	return reqs, nil
}

// Decide resolves a pending approval request: it sets status (which must be a
// terminal decision — approved, denied, or expired), records decided_by (nil
// for an automatic decision such as expiry), and stamps decided_at with now().
// It only acts on a still-pending request, so a second Decide on the same id
// returns [ErrNotFound] rather than overwriting an existing decision; a missing
// id likewise returns [ErrNotFound]. Passing pending is rejected before the DB.
func (s *ApprovalRequestStore) Decide(ctx context.Context, id string, status ApprovalStatus, decidedBy *string) (ApprovalRequest, error) {
	if status == ApprovalStatusPending || !status.Valid() {
		return ApprovalRequest{}, fmt.Errorf("store: decide approval request %q: status must be a terminal decision, got %q", id, status)
	}
	const q = `UPDATE approval_requests
		SET status = $2, decided_by = $3, decided_at = now()
		WHERE id = $1 AND status = 'pending' RETURNING ` + approvalRequestColumns
	req, err := scanApprovalRequest(s.db.QueryRow(ctx, q, id, status, decidedBy))
	if err != nil {
		return ApprovalRequest{}, fmt.Errorf("store: decide approval request %q status=%q: %w", id, status, mapError(err))
	}
	return req, nil
}
