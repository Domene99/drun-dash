package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/store"
)

// sessionStore is the narrow slice of *[store.SessionStore] the session
// handlers use. Naming it as an interface (rather than reaching for the
// concrete store) lets the tests inject an in-memory fake in place of a live
// database while the production path still runs the real SQL.
type sessionStore interface {
	GetByID(ctx context.Context, id string) (store.Session, error)
	ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.Session, error)
}

// newSessionStore builds the [sessionStore] a handler operates through. It is
// the same store-construction seam as newAgentStore: a package-level var so
// tests can swap in a fake without a database; production leaves it at its
// default, which constructs the real store over the request's pool.
var newSessionStore = func(q store.Querier) sessionStore {
	return store.NewSessionStore(q)
}

// apiSession is the OpenAPI Session response schema
// (openapi/v1.yaml #/components/schemas/Session). Per the issue's DTO notes,
// agent_version and terminated_at are omitted when nil, and policy_id /
// policy_version are omitted in v1: the store pins a policy_version_id, not the
// (policy_id, policy_version) pair the schema exposes.
type apiSession struct {
	SessionID     string     `json:"session_id"`
	AgentID       string     `json:"agent_id"`
	AgentVersion  *int       `json:"agent_version,omitempty"`
	Status        string     `json:"status"`
	FanOutCeiling int        `json:"fan_out_ceiling"`
	CreatedAt     time.Time  `json:"created_at"`
	TerminatedAt  *time.Time `json:"terminated_at,omitempty"`
}

// apiSessionList is the OpenAPI SessionList response (a Page envelope plus
// items). next_cursor is nullable: a string on a full page and null once the
// list is exhausted (ADR-0002 §3), so it is a *string emitted without
// omitempty.
type apiSessionList struct {
	Items      []apiSession `json:"items"`
	NextCursor *string      `json:"next_cursor"`
}

// toAPISession maps a store.Session to its API representation (see apiSession
// for the field-level decisions).
func toAPISession(s store.Session) apiSession {
	return apiSession{
		SessionID:     s.ID,
		AgentID:       s.AgentID,
		AgentVersion:  s.AgentVersion,
		Status:        string(s.Status),
		FanOutCeiling: s.FanOutCeiling,
		CreatedAt:     s.CreatedAt,
		TerminatedAt:  s.TerminatedAt,
	}
}

// listSessions implements GET /sessions (operationId listSessions). It returns
// a keyset-paginated page of the calling organization's sessions ordered by
// (created_at, id), per ADR-0002 §1 (org from the token) and §3 (pagination) —
// the same lookahead as listAgents. The contract's optional status filter is
// not applied in v1 (the store exposes no status-scoped page method); an
// unrecognised query parameter is simply ignored.
func (d Deps) listSessions(w http.ResponseWriter, r *http.Request) {
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

	s := newSessionStore(d.Pool)
	sessions, err := s.ListByOrgPage(r.Context(), claims.OrgID, limit+1, afterCreatedAt, afterID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	var next *string
	if len(sessions) > limit {
		sessions = sessions[:limit]
		c := EncodeCursor(Cursor{CreatedAt: sessions[limit-1].CreatedAt, ID: sessions[limit-1].ID})
		next = &c
	}

	items := make([]apiSession, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, toAPISession(s))
	}
	WriteOK(w, apiSessionList{Items: items, NextCursor: next})
}

// getSession implements GET /sessions/{sessionId} (operationId getSession). The
// by-id store lookup is not org-scoped, so after loading the row the handler
// enforces tenant isolation with SameOrg and returns 404 (not 403) on a
// cross-org id, per ADR-0002 §1.
func (d Deps) getSession(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())
	id := chi.URLParam(r, "sessionId")

	s := newSessionStore(d.Pool)
	session, err := s.GetByID(r.Context(), id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, session.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}
	WriteOK(w, toAPISession(session))
}
