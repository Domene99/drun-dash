package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/db"
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
	Update(ctx context.Context, id, name string, description *string) (store.Agent, error)
	SetCurrentVersion(ctx context.Context, id string, version int) (store.Agent, error)
	Delete(ctx context.Context, id string) error
}

// principalReader resolves a member or team by id so setAgentEntitlements can
// confirm every principal in the payload exists and belongs to the caller's
// org. Both *[store.MemberStore] and *[store.TeamStore] expose a GetByID with
// this shape over their own row type; the two seams below adapt each to return
// just the owning org id (the only field the validation needs), which keeps
// this interface uniform across the two principal kinds.
type principalReader interface {
	OrgID(ctx context.Context, id string) (string, error)
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

// newMemberReader and newTeamReader are the principal-validation seams. Each
// wraps the concrete store's GetByID and surfaces only the row's org id, so
// setAgentEntitlements can reject a principal that is missing or lives in
// another org without widening the handler's view of members/teams.
var newMemberReader = func(q store.Querier) principalReader {
	return memberOrgReader{store.NewMemberStore(q)}
}

var newTeamReader = func(q store.Querier) principalReader {
	return teamOrgReader{store.NewTeamStore(q)}
}

type memberOrgReader struct{ s *store.MemberStore }

func (r memberOrgReader) OrgID(ctx context.Context, id string) (string, error) {
	m, err := r.s.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return m.OrgID, nil
}

type teamOrgReader struct{ s *store.TeamStore }

func (r teamOrgReader) OrgID(ctx context.Context, id string) (string, error) {
	t, err := r.s.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return t.OrgID, nil
}

// withEntitlementTx runs fn against an entitlementStore bound to a single
// database transaction, committing on success and rolling back on any error (or
// panic). setAgentEntitlements uses it to make the delete-then-create replace
// atomic: a failure mid-replace leaves the prior grants untouched rather than
// clearing them and stopping half-populated. It is a package var so tests can
// substitute an in-memory fake that runs fn with no pool (the production
// default dereferences pool.Begin, which the fake path skips entirely).
var withEntitlementTx = func(ctx context.Context, pool *db.Pool, fn func(entitlementStore) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(store.NewEntitlementStore(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// withAgentVersionTx runs fn against an agent-version store and an agent store
// bound to a single database transaction, committing on success and rolling
// back on any error (or panic). publishAgentVersion uses it to make the
// append-version-then-advance-pointer sequence atomic: if setting the agent's
// current_version fails, the just-inserted version is rolled back rather than
// left orphaned. It is a package var so tests can substitute an in-memory fake
// that runs fn with no pool (the production default dereferences pool.Begin,
// which the fake path skips entirely), mirroring withEntitlementTx.
var withAgentVersionTx = func(ctx context.Context, pool *db.Pool, fn func(agentVersionStore, agentStore) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(store.NewAgentVersionStore(tx), store.NewAgentStore(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

// apiAgentUpdate is the OpenAPI AgentUpdate request body: every field is
// optional (PATCH). Pointers distinguish an omitted field (nil → keep the
// stored value) from a supplied one. default_policy_id is accepted and ignored
// in v1 (ADR-0002 §4a), consistent with apiAgentCreate.
type apiAgentUpdate struct {
	Name            *string `json:"name"`
	Description     *string `json:"description"`
	DefaultPolicyID *string `json:"default_policy_id"`
}

// apiAgentVersion is the OpenAPI AgentVersion response schema
// (openapi/v1.yaml #/components/schemas/AgentVersion). manifest_digest has no
// backing column and is computed deterministically from the stored manifest
// fields (ADR-0002 §4b, see computeManifestDigest); notes has no column and is
// omitted on read.
type apiAgentVersion struct {
	AgentID        string    `json:"agent_id"`
	Version        int       `json:"version"`
	ManifestDigest string    `json:"manifest_digest"`
	CreatedAt      time.Time `json:"created_at"`
}

// apiAgentVersionList is the OpenAPI AgentVersionList response — a plain {items}
// envelope (a version history is bounded and returned whole, like policies).
type apiAgentVersionList struct {
	Items []apiAgentVersion `json:"items"`
}

// apiAgentVersionCreate is the OpenAPI AgentVersionCreate request body: the
// manifest fields the agent_versions row requires (ADR-0002 §4b). manifest_digest
// is no longer supplied by the client — it is computed server-side from these
// fields and returned on the AgentVersion (see computeManifestDigest). The three
// id pins are optional (nil stores NULL). allowed_tools omitted or empty grants
// no tools. notes has no backing column and is accepted but not persisted.
type apiAgentVersionCreate struct {
	SystemPrompt    string   `json:"system_prompt"`
	AllowedTools    []string `json:"allowed_tools"`
	PolicyVersionID *string  `json:"policy_version_id"`
	EndpointID      *string  `json:"endpoint_id"`
	BudgetID        *string  `json:"budget_id"`
	FanOutCeiling   int      `json:"fan_out_ceiling"`
	Notes           *string  `json:"notes"`
}

// apiEntitlement is the OpenAPI Entitlement schema
// (openapi/v1.yaml #/components/schemas/Entitlement). It is both the response
// shape and (inside apiEntitlementList) the request shape for the PUT replace.
// The principal_type/principal_id pair encodes the store's member-XOR-team pair
// (ADR-0002 §4c); policy_id maps to the store's policy_version_id pin and
// max_fan_out to MaxFanOut, both omitted when nil.
type apiEntitlement struct {
	PrincipalType string  `json:"principal_type"`
	PrincipalID   string  `json:"principal_id"`
	PolicyID      *string `json:"policy_id,omitempty"`
	MaxFanOut     *int    `json:"max_fan_out,omitempty"`
}

// apiEntitlementList is the OpenAPI EntitlementList schema — a plain {items}
// envelope used as both the setAgentEntitlements request and every entitlement
// response.
type apiEntitlementList struct {
	Items []apiEntitlement `json:"items"`
}

const (
	principalTypeUser  = "user"
	principalTypeGroup = "group"
)

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

// computeManifestDigest derives the AgentVersion.manifest_digest the contract
// requires but the schema does not store (ADR-0002 §4b). It hashes a canonical
// JSON serialization of the stored manifest fields — system_prompt,
// allowed_tools, the three pinned ids, and fan_out_ceiling — so identical
// content always yields the same digest and any change to a pinned building
// block changes it. Marshaling a fixed-field struct is deterministic (stable
// field order, no incidental whitespace); allowed_tools is embedded verbatim as
// the stored jsonb. The format ("sha256:" + hex) matches the policy digest.
func computeManifestDigest(v store.AgentVersion) string {
	manifest := struct {
		SystemPrompt    string          `json:"system_prompt"`
		AllowedTools    json.RawMessage `json:"allowed_tools"`
		PolicyVersionID *string         `json:"policy_version_id"`
		EndpointID      *string         `json:"endpoint_id"`
		BudgetID        *string         `json:"budget_id"`
		FanOutCeiling   int             `json:"fan_out_ceiling"`
	}{
		SystemPrompt:    v.SystemPrompt,
		AllowedTools:    json.RawMessage(v.AllowedTools),
		PolicyVersionID: v.PolicyVersionID,
		EndpointID:      v.EndpointID,
		BudgetID:        v.BudgetID,
		FanOutCeiling:   v.FanOutCeiling,
	}
	content, _ := json.Marshal(manifest)
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// toAPIAgentVersion maps a store.AgentVersion to its API representation, with
// manifest_digest computed from the stored manifest (ADR-0002 §4b).
func toAPIAgentVersion(v store.AgentVersion) apiAgentVersion {
	return apiAgentVersion{
		AgentID:        v.AgentID,
		Version:        v.Version,
		ManifestDigest: computeManifestDigest(v),
		CreatedAt:      v.CreatedAt,
	}
}

// toAPIEntitlement maps a store.Entitlement to its API representation
// (ADR-0002 §4c). The table's member-XOR-team pair is exactly one non-nil, so
// the principal mapping is total: MemberID → user, TeamID → group.
func toAPIEntitlement(e store.Entitlement) apiEntitlement {
	a := apiEntitlement{
		PolicyID:  e.PolicyVersionID,
		MaxFanOut: e.MaxFanOut,
	}
	switch {
	case e.MemberID != nil:
		a.PrincipalType = principalTypeUser
		a.PrincipalID = *e.MemberID
	case e.TeamID != nil:
		a.PrincipalType = principalTypeGroup
		a.PrincipalID = *e.TeamID
	}
	return a
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

// updateAgent implements PATCH /agents/{agentId} (operationId updateAgent). It
// loads the agent, enforces tenant isolation (cross-org or missing → 404), and
// applies the supplied fields. AgentUpdate is a partial update: an omitted
// field keeps its stored value, so the handler merges the request onto the
// loaded row before the single-statement store Update. A supplied
// default_policy_id is accepted and ignored (ADR-0002 §4a); a rename colliding
// with an existing (org_id, name) surfaces as 409.
func (d Deps) updateAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "agentId")

	var req apiAgentUpdate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	s := newAgentStore(d.Pool)
	agent, err := s.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	name := agent.Name
	if req.Name != nil {
		if *req.Name == "" {
			WriteValidationError(w, "name", "name must not be empty")
			return
		}
		name = *req.Name
	}
	description := agent.Description
	if req.Description != nil {
		description = req.Description
	}

	updated, err := s.Update(ctx, id, name, description)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteOK(w, toAPIAgent(updated))
}

// deleteAgent implements DELETE /agents/{agentId} (operationId deleteAgent). It
// loads the agent for the tenant-isolation check (cross-org or missing → 404),
// then deletes it and returns 204 with no body. The agent's versions and
// entitlements are removed by the schema's ON DELETE CASCADE.
func (d Deps) deleteAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "agentId")

	s := newAgentStore(d.Pool)
	agent, err := s.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	if err := s.Delete(ctx, id); err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteNoContent(w)
}

// listAgentVersions implements GET /agents/{agentId}/versions (operationId
// listAgentVersions). It confirms the agent belongs to the caller's org (a
// cross-org or missing agent is a 404, never 403) and returns the agent's
// immutable version history whole, ordered by version ascending. Each version's
// manifest_digest is computed at read time (ADR-0002 §4b).
func (d Deps) listAgentVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	agentID := chi.URLParam(r, "agentId")

	agent, err := newAgentStore(d.Pool).GetByID(ctx, agentID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	versions, err := newAgentVersionStore(d.Pool).ListByAgent(ctx, agent.ID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	items := make([]apiAgentVersion, 0, len(versions))
	for _, v := range versions {
		items = append(items, toAPIAgentVersion(v))
	}
	WriteOK(w, apiAgentVersionList{Items: items})
}

// publishAgentVersion implements POST /agents/{agentId}/versions (operationId
// publishAgentVersion). It appends a new immutable version carrying the agent's
// editable config — system prompt, allowed tools, the optional policy/endpoint/
// budget pins, and the fan-out ceiling (ADR-0002 §4b) — then advances the
// agent's current_version pointer to it. The next version number is derived from
// the agent's current_version (+1); a concurrent publish racing for the same
// number loses on the unique (agent_id, version) constraint and surfaces as a
// 409. A cross-org or missing agent is a 404. The insert and the pointer bump
// run in one transaction so a failure never leaves an orphaned version. The
// manifest_digest is computed from the stored fields and returned (not supplied
// by the client). agents.manage is enforced by the router (ADR-0006 §6).
func (d Deps) publishAgentVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	agentID := chi.URLParam(r, "agentId")

	var req apiAgentVersionCreate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.SystemPrompt == "" {
		WriteValidationError(w, "system_prompt", "system_prompt is required")
		return
	}
	if req.FanOutCeiling < 0 {
		WriteValidationError(w, "fan_out_ceiling", "fan_out_ceiling must not be negative")
		return
	}

	agent, err := newAgentStore(d.Pool).GetByID(ctx, agentID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	// allowed_tools is stored verbatim as the jsonb array the digest hashes; a
	// nil/empty slice marshals to "[]", the column's default.
	tools := req.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	next := agent.CurrentVersion + 1
	var created store.AgentVersion
	err = withAgentVersionTx(ctx, d.Pool, func(vs agentVersionStore, as agentStore) error {
		v, err := vs.Create(ctx, store.CreateAgentVersionParams{
			AgentID:         agent.ID,
			Version:         next,
			SystemPrompt:    req.SystemPrompt,
			AllowedTools:    toolsJSON,
			PolicyVersionID: req.PolicyVersionID,
			EndpointID:      req.EndpointID,
			BudgetID:        req.BudgetID,
			FanOutCeiling:   req.FanOutCeiling,
		})
		if err != nil {
			return err
		}
		if _, err := as.SetCurrentVersion(ctx, agent.ID, next); err != nil {
			return err
		}
		created = v
		return nil
	})
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteCreated(w, toAPIAgentVersion(created))
}

// listAgentEntitlements implements GET /agents/{agentId}/entitlements
// (operationId listAgentEntitlements). It confirms the agent belongs to the
// caller's org (cross-org or missing → 404) and returns the agent's grants,
// each mapped to the principal_type/principal_id encoding (ADR-0002 §4c).
func (d Deps) listAgentEntitlements(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	agentID := chi.URLParam(r, "agentId")

	agent, err := newAgentStore(d.Pool).GetByID(ctx, agentID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	ents, err := newEntitlementStore(d.Pool).ListByAgent(ctx, agent.ID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	items := make([]apiEntitlement, 0, len(ents))
	for _, e := range ents {
		items = append(items, toAPIEntitlement(e))
	}
	WriteOK(w, apiEntitlementList{Items: items})
}

// setAgentEntitlements implements PUT /agents/{agentId}/entitlements
// (operationId setAgentEntitlements). It replaces the agent's entire grant set:
// after the tenant-isolation check (cross-org or missing agent → 404) it
// validates every principal in the payload (each must exist and belong to the
// caller's org — an unknown or cross-org principal is a 400, indistinguishable
// so as not to leak another tenant's ids), then transactionally deletes the
// existing grants and creates the new ones (ADR-0002 §4c reverse mapping:
// user → member_id, group → team_id). The resulting grants are returned.
func (d Deps) setAgentEntitlements(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	agentID := chi.URLParam(r, "agentId")

	var req apiEntitlementList
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	agent, err := newAgentStore(d.Pool).GetByID(ctx, agentID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	// Validate every principal and build the create params before touching the
	// database, so a bad payload is rejected without having deleted anything.
	params := make([]store.CreateEntitlementParams, 0, len(req.Items))
	for _, item := range req.Items {
		if item.PrincipalID == "" {
			WriteValidationError(w, "principal_id", "principal_id is required")
			return
		}
		if item.MaxFanOut != nil && *item.MaxFanOut < 0 {
			WriteValidationError(w, "max_fan_out", "max_fan_out must not be negative")
			return
		}

		p := store.CreateEntitlementParams{
			AgentID:         agent.ID,
			PolicyVersionID: item.PolicyID,
			MaxFanOut:       item.MaxFanOut,
		}
		principalID := item.PrincipalID
		switch item.PrincipalType {
		case principalTypeUser:
			if !d.principalInOrg(ctx, newMemberReader(d.Pool), claims.OrgID, principalID) {
				WriteValidationError(w, "principal_id", "unknown principal")
				return
			}
			p.MemberID = &principalID
		case principalTypeGroup:
			if !d.principalInOrg(ctx, newTeamReader(d.Pool), claims.OrgID, principalID) {
				WriteValidationError(w, "principal_id", "unknown principal")
				return
			}
			p.TeamID = &principalID
		default:
			WriteValidationError(w, "principal_type", "principal_type must be \"user\" or \"group\"")
			return
		}
		params = append(params, p)
	}

	var created []store.Entitlement
	err = withEntitlementTx(ctx, d.Pool, func(es entitlementStore) error {
		existing, err := es.ListByAgent(ctx, agent.ID)
		if err != nil {
			return err
		}
		for _, e := range existing {
			if err := es.Delete(ctx, e.ID); err != nil {
				return err
			}
		}
		created = make([]store.Entitlement, 0, len(params))
		for _, p := range params {
			e, err := es.Create(ctx, p)
			if err != nil {
				return err
			}
			created = append(created, e)
		}
		return nil
	})
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	items := make([]apiEntitlement, 0, len(created))
	for _, e := range created {
		items = append(items, toAPIEntitlement(e))
	}
	WriteOK(w, apiEntitlementList{Items: items})
}

// principalInOrg reports whether the principal identified by id exists and
// belongs to orgID, reading through the supplied principalReader (a member or
// team seam). A missing principal (store.ErrNotFound) and a cross-org one both
// return false so the caller reports one indistinguishable "unknown principal"
// validation error, avoiding a cross-tenant existence leak. A genuine store
// failure also returns false; the payload is treated as invalid rather than
// surfacing the raw error.
func (d Deps) principalInOrg(ctx context.Context, reader principalReader, orgID, id string) bool {
	ownerOrg, err := reader.OrgID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		return false
	}
	return SameOrg(orgID, ownerOrg)
}
