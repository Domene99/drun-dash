package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// roleStore is the narrow slice of *[store.RoleStore] the role handlers use.
// Naming it as an interface (rather than reaching for the concrete store) lets
// the tests inject an in-memory fake in place of a live database while the
// production path still runs the real SQL. It is shared with the member-role
// handlers (members.go), which validate a requested role against the org.
type roleStore interface {
	ListByOrg(ctx context.Context, orgID string) ([]store.Role, error)
	Create(ctx context.Context, p store.CreateRoleParams) (store.Role, error)
	GetByID(ctx context.Context, id string) (store.Role, error)
	Update(ctx context.Context, id string, p store.UpdateRoleParams) (store.Role, error)
	SetPermissions(ctx context.Context, id string, perms []store.Permission) error
	SetAgents(ctx context.Context, id string, agents []store.RoleAgentGrant) error
	Delete(ctx context.Context, id string) error
}

// newRoleStore builds the [roleStore] a handler operates through. It is the
// same store-construction seam as newAgentStore: a package-level var so tests
// can swap in a fake without a database; production leaves it at its default,
// which constructs the real store over the request's pool.
var newRoleStore = func(q store.Querier) roleStore {
	return store.NewRoleStore(q)
}

// apiRole is the OpenAPI Role response schema
// (openapi/v1.yaml #/components/schemas/Role, ADR-0006 §7). It carries the
// role's base fields plus its permission set and agent access, so the admin UI
// can render and edit a role in one round trip. builtin is exposed so the UI can
// lock the name/permission controls of a built-in role.
type apiRole struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	Builtin     bool           `json:"builtin"`
	AllAgents   bool           `json:"all_agents"`
	Permissions []string       `json:"permissions"`
	Agents      []apiRoleAgent `json:"agents"`
	CreatedAt   time.Time      `json:"created_at"`
}

// apiRoleAgent is one agent granted by a role
// (#/components/schemas/RoleAgent). It is both the response shape and the
// request shape (inside RoleCreate/RoleUpdate). policy_version_id and
// max_fan_out are the optional per-grant overrides carried over from
// entitlements (ADR-0006 §3), omitted when nil.
type apiRoleAgent struct {
	AgentID         string  `json:"agent_id"`
	PolicyVersionID *string `json:"policy_version_id,omitempty"`
	MaxFanOut       *int    `json:"max_fan_out,omitempty"`
}

// apiRoleList is the OpenAPI RoleList response — a plain {items} envelope. An
// org's roles are a small, bounded set (four built-ins plus custom roles), so
// they are returned whole rather than paginated.
type apiRoleList struct {
	Items []apiRole `json:"items"`
}

// apiRoleCreate is the OpenAPI RoleCreate request body (POST /roles). name is
// required; description, all_agents, permissions, and agents are optional. The
// created role is always custom (builtin = false) — built-in roles are seeded by
// the migration, never created through the API.
type apiRoleCreate struct {
	Name        string         `json:"name"`
	Description *string        `json:"description"`
	AllAgents   bool           `json:"all_agents"`
	Permissions []string       `json:"permissions"`
	Agents      []apiRoleAgent `json:"agents"`
}

// apiRoleUpdate is the OpenAPI RoleUpdate request body (PATCH /roles/{roleId}).
// Every field is a pointer so an omitted field is left unchanged and a supplied
// one replaces the stored value wholesale (ADR-0006 §7: PATCH replaces the
// permission and agent sets). Renaming or changing the permissions of a built-in
// role is rejected with 409; its all_agents/description and agent access stay
// editable.
type apiRoleUpdate struct {
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	AllAgents   *bool           `json:"all_agents"`
	Permissions *[]string       `json:"permissions"`
	Agents      *[]apiRoleAgent `json:"agents"`
}

// toAPIRole maps a store.Role to its API representation. Permissions and Agents
// are always non-nil slices so they serialise as [] rather than null.
func toAPIRole(r store.Role) apiRole {
	perms := make([]string, len(r.Permissions))
	for i, p := range r.Permissions {
		perms[i] = string(p)
	}
	agents := make([]apiRoleAgent, len(r.Agents))
	for i, a := range r.Agents {
		agents[i] = apiRoleAgent{AgentID: a.AgentID, PolicyVersionID: a.PolicyVersionID, MaxFanOut: a.MaxFanOut}
	}
	return apiRole{
		ID:          r.ID,
		Name:        r.Name,
		Description: r.Description,
		Builtin:     r.Builtin,
		AllAgents:   r.AllAgents,
		Permissions: perms,
		Agents:      agents,
		CreatedAt:   r.CreatedAt,
	}
}

// toStorePermissions projects the API permission strings onto the store's
// Permission type. Validation against the catalog is the store's job (it rejects
// an unknown value with store.ErrInvalidPermission → 400).
func toStorePermissions(in []string) []store.Permission {
	out := make([]store.Permission, len(in))
	for i, s := range in {
		out[i] = store.Permission(s)
	}
	return out
}

// writeRoleStoreError maps the RBAC store sentinels on top of the canonical
// ADR-0002 table (WriteStoreError): a bad permission string is a 400, and the
// built-in-immutability / last-owner guards are 409 conflicts (ADR-0006 §1/§7).
func writeRoleStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidPermission):
		WriteValidationError(w, "permissions", "unknown permission")
	case errors.Is(err, store.ErrImmutableBuiltin):
		_ = httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "built-in role cannot be modified or deleted")
	case errors.Is(err, store.ErrLastOwner):
		_ = httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict, "cannot remove the last owner assignment")
	default:
		WriteStoreError(w, err)
	}
}

// buildRoleAgents validates an agent-access payload and projects it onto the
// store's grant type. Every referenced agent must exist and belong to the
// caller's org — an unknown or cross-org agent is a 400 "unknown agent"
// (indistinguishable, so a cross-tenant agent id is not confirmed to exist),
// matching setAgentEntitlements' principal validation. A negative max_fan_out is
// a 400. On any failure it writes the response and returns ok=false.
func (d Deps) buildRoleAgents(w http.ResponseWriter, ctx context.Context, orgID string, items []apiRoleAgent) ([]store.RoleAgentGrant, bool) {
	agents := newAgentStore(d.Pool)
	grants := make([]store.RoleAgentGrant, 0, len(items))
	for _, item := range items {
		if item.AgentID == "" {
			WriteValidationError(w, "agent_id", "agent_id is required")
			return nil, false
		}
		if item.MaxFanOut != nil && *item.MaxFanOut < 0 {
			WriteValidationError(w, "max_fan_out", "max_fan_out must not be negative")
			return nil, false
		}
		agent, err := agents.GetByID(ctx, item.AgentID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				WriteValidationError(w, "agent_id", "unknown agent")
				return nil, false
			}
			WriteStoreError(w, err)
			return nil, false
		}
		if !SameOrg(orgID, agent.OrgID) {
			WriteValidationError(w, "agent_id", "unknown agent")
			return nil, false
		}
		grants = append(grants, store.RoleAgentGrant{
			AgentID:         item.AgentID,
			PolicyVersionID: item.PolicyVersionID,
			MaxFanOut:       item.MaxFanOut,
		})
	}
	return grants, true
}

// listRoles implements GET /roles (operationId listRoles). It returns every
// role in the caller's org — the four built-ins plus any custom roles — with
// their permissions and agent access hydrated (ADR-0006 §7). Requires
// roles.manage (mounted with the gate).
func (d Deps) listRoles(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	roles, err := newRoleStore(d.Pool).ListByOrg(r.Context(), claims.OrgID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	items := make([]apiRole, 0, len(roles))
	for _, role := range roles {
		items = append(items, toAPIRole(role))
	}
	WriteOK(w, apiRoleList{Items: items})
}

// createRole implements POST /roles (operationId createRole). It creates a
// custom role (builtin = false) in the caller's org from the supplied name,
// permissions, and agent access. A duplicate (org_id, name) is a 409, an unknown
// permission a 400, and a cross-org/unknown agent id a 400 (ADR-0006 §7).
func (d Deps) createRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	var req apiRoleCreate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		WriteValidationError(w, "name", "name is required")
		return
	}

	agents, ok := d.buildRoleAgents(w, ctx, claims.OrgID, req.Agents)
	if !ok {
		return
	}

	role, err := newRoleStore(d.Pool).Create(ctx, store.CreateRoleParams{
		OrgID:       claims.OrgID,
		Name:        req.Name,
		Description: req.Description,
		Builtin:     false,
		AllAgents:   req.AllAgents,
		Permissions: toStorePermissions(req.Permissions),
		Agents:      agents,
	})
	if err != nil {
		writeRoleStoreError(w, err)
		return
	}
	WriteCreated(w, toAPIRole(role))
}

// getRole implements GET /roles/{roleId} (operationId getRole). The by-id lookup
// is not org-scoped, so after loading the row the handler enforces tenant
// isolation with SameOrg and returns 404 (not 403) on a cross-org id.
func (d Deps) getRole(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())
	id := chi.URLParam(r, "roleId")

	role, err := newRoleStore(d.Pool).GetByID(r.Context(), id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, role.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}
	WriteOK(w, toAPIRole(role))
}

// updateRole implements PATCH /roles/{roleId} (operationId updateRole). It loads
// the role, enforces tenant isolation (cross-org or missing → 404), and applies
// the supplied fields: base fields (name/description/all_agents) merge onto the
// stored row, and a supplied permissions or agents array replaces the
// corresponding set wholesale (ADR-0006 §7). Renaming or repermissioning a
// built-in role is a 409; an unknown permission a 400. The re-read role is
// returned so the response reflects every applied change.
func (d Deps) updateRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "roleId")

	var req apiRoleUpdate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	rs := newRoleStore(d.Pool)
	role, err := rs.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, role.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	name := role.Name
	if req.Name != nil {
		if *req.Name == "" {
			WriteValidationError(w, "name", "name must not be empty")
			return
		}
		name = *req.Name
	}
	description := role.Description
	if req.Description != nil {
		description = req.Description
	}
	allAgents := role.AllAgents
	if req.AllAgents != nil {
		allAgents = *req.AllAgents
	}

	// Validate the replacement agent set (if provided) before any write, so a
	// bad agent id is rejected without half-applying the update.
	var grants []store.RoleAgentGrant
	if req.Agents != nil {
		g, ok := d.buildRoleAgents(w, ctx, claims.OrgID, *req.Agents)
		if !ok {
			return
		}
		grants = g
	}

	if _, err := rs.Update(ctx, id, store.UpdateRoleParams{Name: name, Description: description, AllAgents: allAgents}); err != nil {
		writeRoleStoreError(w, err)
		return
	}
	if req.Permissions != nil {
		if err := rs.SetPermissions(ctx, id, toStorePermissions(*req.Permissions)); err != nil {
			writeRoleStoreError(w, err)
			return
		}
	}
	if req.Agents != nil {
		if err := rs.SetAgents(ctx, id, grants); err != nil {
			writeRoleStoreError(w, err)
			return
		}
	}

	final, err := rs.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteOK(w, toAPIRole(final))
}

// deleteRole implements DELETE /roles/{roleId} (operationId deleteRole). It
// loads the role for the tenant-isolation check (cross-org or missing → 404),
// then deletes it and returns 204. Deleting a built-in role is a 409
// (ErrImmutableBuiltin, which also protects the Owner role); the
// role_permissions / role_agents / member_roles / team_roles rows cascade.
func (d Deps) deleteRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "roleId")

	rs := newRoleStore(d.Pool)
	role, err := rs.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, role.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}
	if err := rs.Delete(ctx, id); err != nil {
		writeRoleStoreError(w, err)
		return
	}
	WriteNoContent(w)
}
