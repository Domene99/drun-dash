package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// agentStore is the narrow slice of *[store.AgentStore] the agent handlers use.
// Naming it as an interface (rather than reaching for the concrete store) lets
// the tests inject an in-memory fake in place of a live database while the
// production path still runs the real SQL.
type agentStore interface {
	Create(ctx context.Context, orgID, name string, description *string) (store.Agent, error)
	GetByID(ctx context.Context, id string) (store.Agent, error)
	ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.Agent, error)
}

// newAgentStore builds the [agentStore] a handler operates through. It is a
// package-level variable, not a direct call, purely so tests can swap in a fake
// without a database; production leaves it at its default, which constructs the
// real store over the request's pool. This is the store-construction seam every
// later resource handler copies (newPolicyStore, …) — it keeps the store out of
// [Deps] so adding a resource touches only its own handler file.
var newAgentStore = func(q store.Querier) agentStore {
	return store.NewAgentStore(q)
}

// apiAgent is the OpenAPI Agent response schema
// (openapi/v1.yaml #/components/schemas/Agent). The mapping from store.Agent is
// decided in ADR-0002 §4a: latest_version ← CurrentVersion, default_policy_id
// is omitted (no backing column), and a nil description is omitted.
type apiAgent struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   *string   `json:"description,omitempty"`
	LatestVersion int       `json:"latest_version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// apiAgentList is the OpenAPI AgentList response (a Page envelope plus items).
// next_cursor is nullable: it is a string on a full page and null once the list
// is exhausted (ADR-0002 §3), so it is a *string emitted without omitempty.
type apiAgentList struct {
	Items      []apiAgent `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

// apiAgentCreate is the OpenAPI AgentCreate request body. default_policy_id has
// no backing column; per ADR-0002 §4a it is accepted (so a client sending it is
// not rejected with an unknown-field 400) and ignored in v1.
type apiAgentCreate struct {
	Name            string  `json:"name"`
	Description     *string `json:"description"`
	DefaultPolicyID *string `json:"default_policy_id"`
}

// toAPIAgent maps a store.Agent to its API representation (ADR-0002 §4a).
func toAPIAgent(a store.Agent) apiAgent {
	return apiAgent{
		ID:            a.ID,
		Name:          a.Name,
		Description:   a.Description,
		LatestVersion: a.CurrentVersion,
		CreatedAt:     a.CreatedAt,
		UpdatedAt:     a.UpdatedAt,
	}
}

// listAgents implements GET /agents (operationId listAgents). It returns a
// keyset-paginated page of the calling organization's agents ordered by
// (created_at, id), per ADR-0002 §1 (org from the token) and §3 (pagination).
func (d Deps) listAgents(w http.ResponseWriter, r *http.Request) {
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

	s := newAgentStore(d.Pool)
	agents, err := s.ListByOrgPage(r.Context(), claims.OrgID, limit+1, afterCreatedAt, afterID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	var next *string
	if len(agents) > limit {
		agents = agents[:limit]
		c := EncodeCursor(Cursor{CreatedAt: agents[limit-1].CreatedAt, ID: agents[limit-1].ID})
		next = &c
	}

	items := make([]apiAgent, 0, len(agents))
	for _, a := range agents {
		items = append(items, toAPIAgent(a))
	}
	WriteOK(w, apiAgentList{Items: items, NextCursor: next})
}

// createAgent implements POST /agents (operationId createAgent). It registers a
// new agent in the caller's organization (org from the token, never the body).
// A duplicate (org_id, name) surfaces as store.ErrConflict → 409 via
// WriteStoreError, matching the createAgent conflict path in ADR-0002 §2.
func (d Deps) createAgent(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	var req apiAgentCreate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		WriteValidationError(w, "name", "name is required")
		return
	}

	s := newAgentStore(d.Pool)
	agent, err := s.Create(r.Context(), claims.OrgID, req.Name, req.Description)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteCreated(w, toAPIAgent(agent))
}

// getAgent implements GET /agents/{agentId} (operationId getAgent). The by-id
// store lookup is not org-scoped, so after loading the row the handler enforces
// tenant isolation with SameOrg and returns 404 (not 403) on a cross-org id,
// per ADR-0002 §1.
func (d Deps) getAgent(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())
	id := chi.URLParam(r, "agentId")

	s := newAgentStore(d.Pool)
	agent, err := s.GetByID(r.Context(), id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}
	WriteOK(w, toAPIAgent(agent))
}
