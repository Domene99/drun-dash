package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// memberDirectory is the narrow slice of *[store.MemberStore] the member
// handlers use: list the org's members and load one by id for the tenant check.
// As with the other resources it is named as an interface so tests can inject an
// in-memory fake in place of a live database.
type memberDirectory interface {
	GetByID(ctx context.Context, id string) (store.Member, error)
	ListByOrg(ctx context.Context, orgID string) ([]store.Member, error)
}

// newMemberDirectory builds the [memberDirectory] a handler operates through —
// the same package-level-var seam as newAgentStore, so tests swap in a fake and
// production constructs the real store over the request's pool.
var newMemberDirectory = func(q store.Querier) memberDirectory {
	return store.NewMemberStore(q)
}

// memberRoleStore is the narrow slice of *[store.MemberRoleStore] the member
// handlers use: read a member's directly-assigned roles and replace the set by
// assigning/removing individual roles (ADR-0006 §4/§7).
type memberRoleStore interface {
	AssignRole(ctx context.Context, memberID, roleID string) (store.MemberRoleAssignment, error)
	RemoveRole(ctx context.Context, memberID, roleID string) error
	ListRolesForMember(ctx context.Context, memberID string) ([]store.Role, error)
}

// newMemberRoleStore builds the [memberRoleStore] a handler operates through,
// following the same seam pattern as the other stores.
var newMemberRoleStore = func(q store.Querier) memberRoleStore {
	return store.NewMemberRoleStore(q)
}

// apiMember is the OpenAPI Member response schema
// (openapi/v1.yaml #/components/schemas/Member, ADR-0006 §7). It carries the
// member's identity fields plus the roles assigned directly to them, so the
// admin console can render "who holds which roles" in one round trip. Roles a
// member only inherits via a team are not listed here (they are a property of
// the team, editable through the team surface).
type apiMember struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName *string   `json:"display_name,omitempty"`
	Active      bool      `json:"active"`
	Roles       []apiRole `json:"roles"`
}

// apiMemberList is the OpenAPI MemberList response — a plain {items} envelope
// (an org's member roster is bounded and returned whole).
type apiMemberList struct {
	Items []apiMember `json:"items"`
}

// apiMemberRolesUpdate is the OpenAPI MemberRolesUpdate request body
// (PUT /members/{memberId}/roles): the full set of role ids the member should
// hold directly. The handler replaces the member's direct assignments with this
// set (ADR-0006 §7 replace semantics).
type apiMemberRolesUpdate struct {
	RoleIDs []string `json:"role_ids"`
}

// toAPIMember maps a store.Member and its directly-assigned roles to the API
// representation. roles is always a non-nil slice so it serialises as [].
func toAPIMember(m store.Member, roles []store.Role) apiMember {
	items := make([]apiRole, len(roles))
	for i, role := range roles {
		items[i] = toAPIRole(role)
	}
	return apiMember{
		ID:          m.ID,
		Email:       m.Email,
		DisplayName: m.DisplayName,
		Active:      m.Active,
		Roles:       items,
	}
}

// listMembers implements GET /members (operationId listMembers). It returns the
// calling org's members, each with the roles assigned directly to them
// (ADR-0006 §7). Visible to members.manage OR audit.view (mounted with the
// gate). The roster is bounded, so it is returned whole rather than paginated.
func (d Deps) listMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	members, err := newMemberDirectory(d.Pool).ListByOrg(ctx, claims.OrgID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	mrs := newMemberRoleStore(d.Pool)
	items := make([]apiMember, 0, len(members))
	for _, m := range members {
		roles, err := mrs.ListRolesForMember(ctx, m.ID)
		if err != nil {
			WriteStoreError(w, err)
			return
		}
		items = append(items, toAPIMember(m, roles))
	}
	WriteOK(w, apiMemberList{Items: items})
}

// updateMemberRoles implements PUT /members/{memberId}/roles (operationId
// updateMemberRoles). It replaces the member's directly-assigned role set with
// the supplied ids. The member must belong to the caller's org (cross-org or
// missing → 404), and every requested role must exist in the org (a cross-org or
// unknown roleId is a 404). It then removes the roles no longer desired and
// assigns the newly desired ones; the store's last-owner guard surfaces as a 409
// if a removal would leave the org without an Owner (ADR-0006 §7). Requires
// members.manage (mounted with the gate).
func (d Deps) updateMemberRoles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	memberID := chi.URLParam(r, "memberId")

	var req apiMemberRolesUpdate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	member, err := newMemberDirectory(d.Pool).GetByID(ctx, memberID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, member.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	// Validate every requested role exists and belongs to the org before any
	// write, deduping the desired set. A cross-org or unknown roleId is a 404.
	rs := newRoleStore(d.Pool)
	desired := make(map[string]bool, len(req.RoleIDs))
	for _, roleID := range req.RoleIDs {
		if roleID == "" {
			WriteValidationError(w, "role_ids", "role id must not be empty")
			return
		}
		role, err := rs.GetByID(ctx, roleID)
		if err != nil {
			WriteStoreError(w, err)
			return
		}
		if !SameOrg(claims.OrgID, role.OrgID) {
			WriteNotFound(w, "resource not found")
			return
		}
		desired[roleID] = true
	}

	mrs := newMemberRoleStore(d.Pool)
	current, err := mrs.ListRolesForMember(ctx, memberID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	currentSet := make(map[string]bool, len(current))
	for _, role := range current {
		currentSet[role.ID] = true
	}

	// Remove first (so the last-owner guard sees the true post-state), then add.
	for _, role := range current {
		if !desired[role.ID] {
			if err := mrs.RemoveRole(ctx, memberID, role.ID); err != nil {
				writeRoleStoreError(w, err)
				return
			}
		}
	}
	for roleID := range desired {
		if !currentSet[roleID] {
			if _, err := mrs.AssignRole(ctx, memberID, roleID); err != nil {
				writeRoleStoreError(w, err)
				return
			}
		}
	}

	final, err := mrs.ListRolesForMember(ctx, memberID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteOK(w, toAPIMember(member, final))
}
