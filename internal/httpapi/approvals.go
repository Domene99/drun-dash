package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// approvalStore is the narrow slice of *[store.ApprovalRequestStore] the
// approval handlers use. Naming it as an interface (rather than reaching for
// the concrete store) lets the tests inject an in-memory fake in place of a
// live database while the production path still runs the real SQL.
type approvalStore interface {
	Create(ctx context.Context, p store.CreateApprovalRequestParams) (store.ApprovalRequest, error)
	GetByID(ctx context.Context, id string) (store.ApprovalRequest, error)
	ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.ApprovalRequest, error)
	Decide(ctx context.Context, id string, status store.ApprovalStatus, decidedBy *string) (store.ApprovalRequest, error)
}

// newApprovalStore builds the [approvalStore] a handler operates through. It is
// the same store-construction seam as newAgentStore/newSessionStore: a
// package-level var so tests can swap in a fake without a database; production
// leaves it at its default, which constructs the real store over the request's
// pool.
var newApprovalStore = func(q store.Querier) approvalStore {
	return store.NewApprovalRequestStore(q)
}

// apiApproval is the OpenAPI Approval response schema
// (openapi/v1.yaml #/components/schemas/Approval). reason, decided_by, and
// decided_at are omitted when nil (a still-pending request has no decision).
// The store's context_json is deliberately not surfaced here: the response
// schema carries no context field (it is a create-only input).
type apiApproval struct {
	ID        string     `json:"id"`
	SessionID string     `json:"session_id"`
	Action    string     `json:"action"`
	Reason    *string    `json:"reason,omitempty"`
	Status    string     `json:"status"`
	DecidedBy *string    `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// apiApprovalList is the OpenAPI ApprovalList response (a Page envelope plus
// items). next_cursor is nullable: a string on a full page and null once the
// list is exhausted (ADR-0002 §3), so it is a *string emitted without
// omitempty.
type apiApprovalList struct {
	Items      []apiApproval `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

// apiApprovalCreate is the OpenAPI ApprovalCreate request body. context is a
// free-form JSON object passed straight through to context_json (a nil value
// stores the column default, '{}').
type apiApprovalCreate struct {
	SessionID string          `json:"session_id"`
	Action    string          `json:"action"`
	Reason    *string         `json:"reason"`
	Context   json.RawMessage `json:"context"`
}

// apiApprovalDecision is the OpenAPI ApprovalDecision request body. decision is
// the required verdict; reason is an optional rationale for the audit trail
// that the store's Decide does not persist (there is no backing column), so it
// is accepted and ignored in v1, mirroring how createAgent handles
// default_policy_id.
type apiApprovalDecision struct {
	Decision string  `json:"decision"`
	Reason   *string `json:"reason"`
}

// toAPIApproval maps a store.ApprovalRequest to its API representation (see
// apiApproval for the field-level decisions).
func toAPIApproval(a store.ApprovalRequest) apiApproval {
	return apiApproval{
		ID:        a.ID,
		SessionID: a.SessionID,
		Action:    a.Action,
		Reason:    a.Reason,
		Status:    string(a.Status),
		DecidedBy: a.DecidedBy,
		DecidedAt: a.DecidedAt,
		CreatedAt: a.CreatedAt,
	}
}

// listApprovals implements GET /approvals (operationId listApprovals). It
// returns a keyset-paginated page of the calling organization's approval
// requests ordered by (created_at, id), per ADR-0002 §1 (org from the token)
// and §3 (pagination) — the same lookahead as listAgents. The contract's
// optional status filter is not applied in v1 (the paged store method is not
// status-scoped); an unrecognised query parameter is simply ignored.
func (d Deps) listApprovals(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	limit, err := ParseLimit(r)
	if err != nil {
		WriteValidationError(w, "limit", "limit must be an integer")
		return
	}
	cursor, err := DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		WriteValidationError(w, "cursor", "invalid cursor")
		return
	}

	// A nil cursor leaves the seek position zero, which ListByOrgPage reads as
	// "from the beginning". Request limit+1 as a lookahead to detect a next
	// page without a second query.
	var afterCreatedAt time.Time
	var afterID string
	if cursor != nil {
		afterCreatedAt = cursor.CreatedAt
		afterID = cursor.ID
	}

	s := newApprovalStore(d.Pool)
	approvals, err := s.ListByOrgPage(r.Context(), claims.OrgID, limit+1, afterCreatedAt, afterID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	var next *string
	if len(approvals) > limit {
		approvals = approvals[:limit]
		c := EncodeCursor(Cursor{CreatedAt: approvals[limit-1].CreatedAt, ID: approvals[limit-1].ID})
		next = &c
	}

	items := make([]apiApproval, 0, len(approvals))
	for _, a := range approvals {
		items = append(items, toAPIApproval(a))
	}
	WriteOK(w, apiApprovalList{Items: items, NextCursor: next})
}

// createApproval implements POST /approvals (operationId createApproval). It
// raises a pending approval request for a session in the caller's organization.
// session_id and action are required (400 otherwise); the referenced session is
// loaded and checked against the caller's org, so a cross-org or missing
// session is a 404 (never 403) and cannot seed a request under another tenant.
// The org and pending status come from the server, never the body.
func (d Deps) createApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	var req apiApprovalCreate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.SessionID == "" {
		WriteValidationError(w, "session_id", "session_id is required")
		return
	}
	if req.Action == "" {
		WriteValidationError(w, "action", "action is required")
		return
	}

	// Confirm the referenced session belongs to the caller's org before
	// creating anything: a cross-org or missing session is a 404 so nothing
	// leaks across tenants and the foreign key never sees a foreign id.
	session, err := newSessionStore(d.Pool).GetByID(ctx, req.SessionID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, session.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	s := newApprovalStore(d.Pool)
	approval, err := s.Create(ctx, store.CreateApprovalRequestParams{
		OrgID:       claims.OrgID,
		SessionID:   req.SessionID,
		Action:      req.Action,
		Reason:      req.Reason,
		ContextJSON: req.Context,
	})
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteCreated(w, toAPIApproval(approval))
}

// decideApproval implements POST /approvals/{approvalId}/decision (operationId
// decideApproval). It records the approver's verdict — approve→approved,
// deny→denied — on a pending request. The request is loaded org-scoped (a
// cross-org or missing id is a 404); a request that is no longer pending is a
// 409 conflict, the contract-sanctioned status for a re-decision (ADR-0002 §2).
// The decision reason is accepted but not persisted (no backing column).
func (d Deps) decideApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "approvalId")

	var req apiApprovalDecision
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	var status store.ApprovalStatus
	switch req.Decision {
	case "approve":
		status = store.ApprovalStatusApproved
	case "deny":
		status = store.ApprovalStatusDenied
	default:
		WriteValidationError(w, "decision", "decision must be one of: approve, deny")
		return
	}

	s := newApprovalStore(d.Pool)
	approval, err := s.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, approval.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}
	if approval.Status != store.ApprovalStatusPending {
		_ = httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "approval request already decided")
		return
	}

	// decided_by records the principal that ruled. A member id is always
	// present on an authenticated principal, but guard against an empty subject
	// so the decided_by foreign key never sees an empty string.
	var decidedBy *string
	if claims.MemberID != "" {
		mid := claims.MemberID
		decidedBy = &mid
	}
	decided, err := s.Decide(ctx, id, status, decidedBy)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteOK(w, toAPIApproval(decided))
}
