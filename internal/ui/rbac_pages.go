package ui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/ui/views"
)

// This file wires the RBAC governance pages (Roles, Members) and the
// read-only Agents/Policies detail views onto the control-plane client
// (ADR-0006 §7). Management actions are permission-gated from the caller's
// effective permissions (GET /auth/whoami) so the UI never offers an action the
// server would reject — the server still enforces; this is the UX layer.

// The permission verbs this package gates management actions on. They mirror the
// application catalog (ADR-0006 §2, internal/store/permissions.go); the admin UI
// is a separate binary and never imports the control-plane store, so the small
// set it needs is restated here.
const (
	permRolesManage     = "roles.manage"
	permMembersManage   = "members.manage"
	permAuditView       = "audit.view"
	permAgentsManage    = "agents.manage"
	permPoliciesManage  = "policies.manage"
	permEndpointsManage = "endpoints.manage"
)

// hasPermission reports whether perms contains want.
func hasPermission(perms []string, want string) bool {
	for _, p := range perms {
		if p == want {
			return true
		}
	}
	return false
}

// permissionCatalog is the role-editor permission checklist: the application
// permission catalog (ADR-0006 §2) with a human description of each verb, in
// catalog order. Restated here (not imported from the store) since the admin UI
// never imports control-plane packages.
func permissionCatalog() []views.PermissionOption {
	return []views.PermissionOption{
		{Key: "agents.manage", Description: "Create, update, delete, and publish agents"},
		{Key: "policies.manage", Description: "Create policies and publish policy versions"},
		{Key: "endpoints.manage", Description: "Manage model endpoints"},
		{Key: "budgets.manage", Description: "Manage budgets and budget rules"},
		{Key: "members.manage", Description: "Manage members and assign roles"},
		{Key: "roles.manage", Description: "Create, update, and delete roles"},
		{Key: "approvals.decide", Description: "Approve and deny escalation requests"},
		{Key: "audit.view", Description: "View the audit log, spend, and all sessions"},
		{Key: "agents.run", Description: "Launch sessions for the agents this role grants"},
	}
}

// --- Roles ----------------------------------------------------------------

// handleRoles renders the Roles page. It reports whether the caller may manage
// roles (roles.manage) so the view offers or hides the create/edit/delete
// controls. Without roles.manage the control plane rejects the roles API, so the
// page shows a view-only notice instead of an error.
func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, perms, err := s.buildChrome(r.Context(), token, "roles", "Roles")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.rolesData(r.Context(), token, chrome, perms, "")
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.Roles(data))
}

// rolesData assembles the Roles view model: the manage flag, the permission
// catalog and agent options for the forms, and the role rows. Section failures
// are embedded as data.Error; only a rejected token is returned as
// ErrUnauthorized so the caller can redirect. formError seeds a message after a
// failed write.
func (s *Server) rolesData(ctx context.Context, token string, chrome views.Chrome, perms []string, formError string) (views.RolesData, error) {
	data := views.RolesData{Chrome: chrome, CanManage: hasPermission(perms, permRolesManage), Error: formError}
	if !data.CanManage {
		data.NoAccess = true
		return data, nil
	}
	data.Permissions = permissionCatalog()

	agents, err := s.client.ListAgents(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.RolesData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list agents for roles failed", slog.Any("error", err))
	default:
		data.Agents = agentOptions(agents.Items)
	}

	roles, err := s.client.ListRoles(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.RolesData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list roles failed", slog.Any("error", err))
		if data.Error == "" {
			data.Error = "Could not load roles: " + readableError(err)
		}
	default:
		data.Roles = roleViews(roles.Items, s.now())
	}
	return data, nil
}

// handleRoleCreate creates a custom role from the submitted form and returns to
// the Roles page. A rejected token redirects to /login; a validation or
// conflict error re-renders the page with the message.
func (s *Server) handleRoleCreate(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderRolesError(w, r, token, "Could not read the submitted form.")
		return
	}
	body := RoleCreate{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		AllAgents:   r.PostFormValue("all_agents") != "",
		Permissions: r.PostForm["permissions"],
		Agents:      roleAgentsFromForm(r.PostForm["agents"]),
	}
	if body.Name == "" {
		s.renderRolesError(w, r, token, "A role name is required.")
		return
	}
	_, err := s.client.CreateRole(r.Context(), token, body)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: create role failed", slog.Any("error", err))
		s.renderRolesError(w, r, token, "Could not create role: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/roles", http.StatusSeeOther)
}

// handleRoleUpdate replaces a custom role's fields with the submitted form
// (PATCH replace semantics) and returns to the Roles page. Every field is sent,
// so a cleared checklist clears that set.
func (s *Server) handleRoleUpdate(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	roleID := chi.URLParam(r, "roleId")
	if err := r.ParseForm(); err != nil {
		s.renderRolesError(w, r, token, "Could not read the submitted form.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.renderRolesError(w, r, token, "A role name is required.")
		return
	}
	description := strings.TrimSpace(r.PostFormValue("description"))
	allAgents := r.PostFormValue("all_agents") != ""
	permissions := r.PostForm["permissions"]
	if permissions == nil {
		permissions = []string{}
	}
	agents := roleAgentsFromForm(r.PostForm["agents"])
	body := RoleUpdate{
		Name:        &name,
		Description: &description,
		AllAgents:   &allAgents,
		Permissions: &permissions,
		Agents:      &agents,
	}
	_, err := s.client.UpdateRole(r.Context(), token, roleID, body)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: update role failed", slog.Any("error", err))
		s.renderRolesError(w, r, token, "Could not update role: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/roles", http.StatusSeeOther)
}

// handleRoleDelete deletes a custom role and returns to the Roles page. A
// built-in role is a 409 surfaced as an inline message.
func (s *Server) handleRoleDelete(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	roleID := chi.URLParam(r, "roleId")
	err := s.client.DeleteRole(r.Context(), token, roleID)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: delete role failed", slog.Any("error", err))
		s.renderRolesError(w, r, token, "Could not delete role: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/roles", http.StatusSeeOther)
}

// renderRolesError re-renders the Roles page carrying a message after a failed
// write, rebuilding the live data so the table still reflects current state.
func (s *Server) renderRolesError(w http.ResponseWriter, r *http.Request, token, msg string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "roles", "Roles")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.rolesData(r.Context(), token, chrome, perms, msg)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.Roles(data))
}

// --- Members ---------------------------------------------------------------

// handleMembers renders the Members page. The roster is visible to
// members.manage or audit.view; the assign-roles action is offered only with
// members.manage.
func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, perms, err := s.buildChrome(r.Context(), token, "members", "Members")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.membersData(r.Context(), token, chrome, perms, "")
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.Members(data))
}

// membersData assembles the Members view model. Only a rejected token is
// returned as ErrUnauthorized; a load failure is embedded as data.Error.
func (s *Server) membersData(ctx context.Context, token string, chrome views.Chrome, perms []string, formError string) (views.MembersData, error) {
	data := views.MembersData{Chrome: chrome, CanManage: hasPermission(perms, permMembersManage), Error: formError}
	if !data.CanManage && !hasPermission(perms, permAuditView) {
		data.NoAccess = true
		return data, nil
	}

	members, err := s.client.ListMembers(ctx, token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.MembersData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: list members failed", slog.Any("error", err))
		if data.Error == "" {
			data.Error = "Could not load members: " + readableError(err)
		}
		return data, nil
	default:
		data.Members = memberViews(members.Items)
		if data.CanManage {
			data.Roles = s.assignableRoles(ctx, token, members.Items)
		}
	}
	return data, nil
}

// assignableRoles lists the org's roles for the assign-roles control. It prefers
// the full catalog (GET /roles), but that requires roles.manage; a members
// manager without it falls back to the union of roles already assigned to
// members, so the control still works for the common reassignment case.
func (s *Server) assignableRoles(ctx context.Context, token string, members []Member) []views.RoleOption {
	if roles, err := s.client.ListRoles(ctx, token); err == nil {
		out := make([]views.RoleOption, 0, len(roles.Items))
		for _, role := range roles.Items {
			out = append(out, views.RoleOption{ID: role.ID, Name: role.Name})
		}
		return out
	}
	seen := map[string]bool{}
	var out []views.RoleOption
	for _, m := range members {
		for _, role := range m.Roles {
			if !seen[role.ID] {
				seen[role.ID] = true
				out = append(out, views.RoleOption{ID: role.ID, Name: role.Name})
			}
		}
	}
	return out
}

// handleMemberRoles replaces a member's directly-assigned role set with the
// submitted selection and returns to the Members page.
func (s *Server) handleMemberRoles(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	memberID := chi.URLParam(r, "memberId")
	if err := r.ParseForm(); err != nil {
		s.renderMembersError(w, r, token, "Could not read the submitted form.")
		return
	}
	roleIDs := r.PostForm["role_ids"]
	_, err := s.client.UpdateMemberRoles(r.Context(), token, memberID, roleIDs)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: update member roles failed", slog.Any("error", err))
		s.renderMembersError(w, r, token, "Could not assign roles: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/members", http.StatusSeeOther)
}

// renderMembersError re-renders the Members page with a message after a failed
// role assignment.
func (s *Server) renderMembersError(w http.ResponseWriter, r *http.Request, token, msg string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "members", "Members")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.membersData(r.Context(), token, chrome, perms, msg)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.Members(data))
}

// --- Agents (list + read-only detail) --------------------------------------

// handleAgents renders the agent catalog list, the entry point into the
// read-only agent detail views.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, perms, err := s.buildChrome(r.Context(), token, "agents", "Agents")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.AgentsData{Chrome: chrome, CanManage: hasPermission(perms, permAgentsManage)}
	agents, err := s.client.ListAgents(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list agents failed", slog.Any("error", err))
		data.Error = "Could not load agents: " + readableError(err)
	default:
		data.Agents = agentListRows(agents.Items, s.now())
	}
	s.render(w, r, http.StatusOK, views.Agents(data))
}

// handleAgentDetail renders the read-only detail view for one agent: its
// metadata (default policy resolved to a NAME), its versions, the roles that
// grant access, and the principals entitled to launch it. Sub-sections are
// best-effort so one degraded endpoint does not blank the page.
func (s *Server) handleAgentDetail(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	agentID := chi.URLParam(r, "agentId")
	chrome, perms, err := s.buildChrome(r.Context(), token, "agents", "Agent")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.AgentDetailData{Chrome: chrome, ID: agentID, CanManage: hasPermission(perms, permAgentsManage)}

	agent, err := s.client.GetAgent(r.Context(), token, agentID)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: get agent failed", slog.Any("error", err))
		data.Error = "Could not load agent: " + readableError(err)
		s.render(w, r, http.StatusOK, views.AgentDetail(data))
		return
	}

	now := s.now()
	data.Name = agent.Name
	data.Description = agent.Description
	data.LatestVersion = versionText(agent.LatestVersion)
	data.DefaultPolicyID = agent.DefaultPolicyID
	data.Created = relativeTime(now, agent.CreatedAt)
	data.Updated = relativeTime(now, agent.UpdatedAt)
	if agent.DefaultPolicyID != "" {
		data.DefaultPolicy = s.policyName(r.Context(), token, agent.DefaultPolicyID)
	}

	if versions, err := s.client.ListAgentVersions(r.Context(), token, agentID); err != nil {
		s.logBestEffort(r.Context(), "admin-ui: list agent versions failed", err)
	} else {
		data.Versions = agentVersionRows(versions.Items, now)
	}

	if ents, err := s.client.ListAgentEntitlements(r.Context(), token, agentID); err != nil {
		s.logBestEffort(r.Context(), "admin-ui: list agent entitlements failed", err)
	} else {
		data.Entitlements = entitlementRows(ents.Items)
	}

	// Which roles grant this agent is a property of the roles, readable only
	// with roles.manage; otherwise leave a note rather than an empty list.
	if hasPermission(perms, permRolesManage) {
		if roles, err := s.client.ListRoles(r.Context(), token); err != nil {
			s.logBestEffort(r.Context(), "admin-ui: list roles for agent access failed", err)
		} else {
			data.Access = agentRoleAccess(roles.Items, agentID)
		}
	} else {
		data.AccessNote = "Viewing which roles grant this agent requires the roles.manage permission."
	}

	s.render(w, r, http.StatusOK, views.AgentDetail(data))
}

// policyName resolves a policy id to its readable name, best-effort (empty on
// any failure so the caller degrades gracefully).
func (s *Server) policyName(ctx context.Context, token, id string) string {
	if p, err := s.client.GetPolicy(ctx, token, id); err == nil {
		return p.Name
	}
	return ""
}

// policyRoleOptions lists the org's roles for the policy create dialog's
// permitted-roles checklist, best-effort (nil on failure so the dialog still
// renders, just without the checklist).
func (s *Server) policyRoleOptions(ctx context.Context, token string) []views.RoleOption {
	list, err := s.client.ListRoles(ctx, token)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			s.logger.ErrorContext(ctx, "admin-ui: list roles for policy create failed", slog.Any("error", err))
		}
		return nil
	}
	return roleOptions(list.Items)
}

// --- Policies (list + read-only detail) ------------------------------------

// handlePolicies renders the policy catalog list, the entry point into the
// read-only policy detail views.
func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, perms, err := s.buildChrome(r.Context(), token, "policies", "Policies")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.PoliciesData{Chrome: chrome, CanManage: hasPermission(perms, permPoliciesManage)}
	if data.CanManage {
		data.Roles = s.policyRoleOptions(r.Context(), token)
	}
	policies, err := s.client.ListPolicies(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list policies failed", slog.Any("error", err))
		data.Error = "Could not load policies: " + readableError(err)
	default:
		data.Policies = policyListRows(policies.Items, s.now())
	}
	s.render(w, r, http.StatusOK, views.Policies(data))
}

// handlePolicyCreate creates a policy and its first version from the submitted
// form and returns to the Policies page. A rejected token redirects to /login; a
// validation problem or a duplicate name (409) re-renders the page with the
// message.
func (s *Server) handlePolicyCreate(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderPoliciesError(w, r, token, "Could not read the submitted form.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.renderPoliciesError(w, r, token, "A policy name is required.")
		return
	}
	boundary := PolicyBoundary{
		AllowedTools: parseToolList(r.PostFormValue("allowed_tools")),
		Network:      strings.TrimSpace(r.PostFormValue("network")),
	}
	if raw := strings.TrimSpace(r.PostFormValue("fan_out_ceiling")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			s.renderPoliciesError(w, r, token, "Fan-out ceiling must be a whole number of 0 or more.")
			return
		}
		boundary.FanOutCeiling = &n
	}
	// Pre-authorized egress hosts. Persist them whenever any are supplied,
	// regardless of the network posture — the engine applies the allowlist only
	// when network=allowlist (internal/policycompile). Leaving Rules unset for an
	// empty list keeps the boundary clean.
	if hosts := parseToolList(r.PostFormValue("network_allowlist")); len(hosts) > 0 {
		boundary.Rules = map[string]any{"network_allowlist": toAnySlice(hosts)}
	}
	body := PolicyCreate{
		Name:        name,
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Boundary:    boundary,
	}

	policy, err := s.client.CreatePolicy(r.Context(), token, body)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: create policy failed", slog.Any("error", err))
		s.renderPoliciesError(w, r, token, "Could not create policy: "+readableError(err))
		return
	}
	// Permitted roles are set separately (PUT /policies/{id}/roles). Publishing a
	// signed boundary version is out of the browser's reach, but granting roles is
	// free — so a create can seed the policy's permitted roles in one flow.
	if roleIDs := r.PostForm["permitted_roles"]; len(roleIDs) > 0 {
		if err := s.client.SetPolicyRoles(r.Context(), token, policy.ID, roleIDs); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				s.redirectToLogin(w, r)
				return
			}
			s.logger.ErrorContext(r.Context(), "admin-ui: set policy roles on create failed", slog.Any("error", err))
			s.renderPoliciesError(w, r, token, "Policy created, but its permitted roles could not be saved: "+readableError(err))
			return
		}
	}
	http.Redirect(w, r, "/policies", http.StatusSeeOther)
}

// renderPoliciesError re-renders the Policies page carrying a message after a
// failed create, rebuilding the live list so the table still reflects current
// state.
func (s *Server) renderPoliciesError(w http.ResponseWriter, r *http.Request, token, msg string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "policies", "Policies")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.PoliciesData{Chrome: chrome, CanManage: hasPermission(perms, permPoliciesManage), Error: msg}
	if data.CanManage {
		data.Roles = s.policyRoleOptions(r.Context(), token)
	}
	policies, err := s.client.ListPolicies(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: list policies failed", slog.Any("error", err))
	default:
		data.Policies = policyListRows(policies.Items, s.now())
	}
	s.render(w, r, http.StatusOK, views.Policies(data))
}

// handlePolicyDetail renders the detail view for one policy: its metadata, its
// permitted roles (with an edit affordance for policies.manage), and its
// immutable signed versions.
func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	policyID := chi.URLParam(r, "policyId")
	chrome, perms, err := s.buildChrome(r.Context(), token, "policies", "Policy")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.policyDetailData(r.Context(), token, chrome, perms, policyID, "")
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.PolicyDetail(data))
}

// policyDetailData assembles the policy-detail view model: the policy metadata,
// its permitted roles (resolved ids->names via the roles catalog and carried
// with the full catalog + current selection so a manager can edit the set), and
// its signed versions. Only a rejected token is returned as ErrUnauthorized;
// section failures degrade in place. formError seeds a message after a failed
// permitted-roles write.
func (s *Server) policyDetailData(ctx context.Context, token string, chrome views.Chrome, perms []string, policyID, formError string) (views.PolicyDetailData, error) {
	data := views.PolicyDetailData{Chrome: chrome, ID: policyID, CanManage: hasPermission(perms, permPoliciesManage), Error: formError}

	policy, err := s.client.GetPolicy(ctx, token, policyID)
	switch {
	case errors.Is(err, ErrUnauthorized):
		return views.PolicyDetailData{}, err
	case err != nil:
		s.logger.ErrorContext(ctx, "admin-ui: get policy failed", slog.Any("error", err))
		if data.Error == "" {
			data.Error = "Could not load policy: " + readableError(err)
		}
		return data, nil
	}

	now := s.now()
	data.Name = policy.Name
	data.Description = policy.Description
	data.CurrentVersion = strconv.Itoa(policy.CurrentVersion)
	data.Created = relativeTime(now, policy.CreatedAt)

	// Permitted roles (policy-centric access, ADR-0010). Resolving ids to names
	// needs the roles catalog (roles.manage); both calls are best-effort so a
	// missing permission leaves the section informative rather than blanking it.
	var roles []Role
	if list, err := s.client.ListRoles(ctx, token); err != nil {
		s.logBestEffort(ctx, "admin-ui: list roles for policy access failed", err)
	} else {
		roles = list.Items
		data.AllRoles = roleOptions(roles)
	}
	if ids, err := s.client.GetPolicyRoles(ctx, token, policyID); err != nil {
		s.logBestEffort(ctx, "admin-ui: get policy roles failed", err)
	} else {
		data.PermittedRoleIDs = ids
		data.PermittedRoles = policyPermittedRoles(roles, ids)
	}

	if versions, err := s.client.ListPolicyVersions(ctx, token, policyID); err != nil {
		s.logBestEffort(ctx, "admin-ui: list policy versions failed", err)
	} else {
		data.Versions = policyVersionRows(versions.Items, now)
	}
	return data, nil
}

// handlePolicyRoles replaces a policy's permitted-role set with the submitted
// selection (PUT /policies/{policyId}/roles) and returns to the policy detail
// page. This is the one policy edit the UI performs: granting roles is free and
// unsigned, whereas publishing a new boundary version needs a policyctl
// signature the browser cannot produce. Requires policies.manage.
func (s *Server) handlePolicyRoles(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	policyID := chi.URLParam(r, "policyId")
	if err := r.ParseForm(); err != nil {
		s.renderPolicyDetailError(w, r, token, policyID, "Could not read the submitted form.")
		return
	}
	err := s.client.SetPolicyRoles(r.Context(), token, policyID, r.PostForm["permitted_roles"])
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	if err != nil {
		s.logger.ErrorContext(r.Context(), "admin-ui: set policy roles failed", slog.Any("error", err))
		s.renderPolicyDetailError(w, r, token, policyID, "Could not update permitted roles: "+readableError(err))
		return
	}
	http.Redirect(w, r, "/policies/"+policyID, http.StatusSeeOther)
}

// renderPolicyDetailError re-renders the policy detail page carrying a message
// after a failed permitted-roles write, rebuilding the live data so the page
// still reflects current state.
func (s *Server) renderPolicyDetailError(w http.ResponseWriter, r *http.Request, token, policyID, msg string) {
	chrome, perms, err := s.buildChrome(r.Context(), token, "policies", "Policy")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data, err := s.policyDetailData(r.Context(), token, chrome, perms, policyID, msg)
	if errors.Is(err, ErrUnauthorized) {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.PolicyDetail(data))
}

// logBestEffort logs a non-fatal sub-section failure on a detail page (the page
// still renders). A rejected token here is not turned into a redirect: the page
// shell already authenticated via buildChrome, so a stray 401 on a secondary
// call is just logged and its section left empty.
func (s *Server) logBestEffort(ctx context.Context, msg string, err error) {
	s.logger.ErrorContext(ctx, msg, slog.Any("error", err))
}

// --- view-model mapping helpers --------------------------------------------

// agentOptions maps agents to the role-editor's selectable agent options.
func agentOptions(agents []Agent) []views.AgentOption {
	out := make([]views.AgentOption, 0, len(agents))
	for _, a := range agents {
		out = append(out, views.AgentOption{ID: a.ID, Name: a.Name})
	}
	return out
}

// roleViews maps roles to Roles-table rows, carrying the state the edit form
// pre-fills (permission keys, granted agent ids) and a human agent-access
// summary.
func roleViews(roles []Role, now time.Time) []views.RoleView {
	out := make([]views.RoleView, 0, len(roles))
	for _, role := range roles {
		agentIDs := make([]string, 0, len(role.Agents))
		for _, g := range role.Agents {
			agentIDs = append(agentIDs, g.AgentID)
		}
		out = append(out, views.RoleView{
			ID:              role.ID,
			Name:            role.Name,
			Description:     role.Description,
			Builtin:         role.Builtin,
			PermissionCount: strconv.Itoa(len(role.Permissions)),
			PermissionKeys:  append([]string(nil), role.Permissions...),
			AllAgents:       role.AllAgents,
			AgentIDs:        agentIDs,
			AccessSummary:   agentAccessSummary(role.AllAgents, len(agentIDs)),
			Created:         relativeTime(now, role.CreatedAt),
		})
	}
	return out
}

// agentAccessSummary renders a role's agent-access column: "All agents" when the
// role grants everything, otherwise a count of granted agents.
func agentAccessSummary(allAgents bool, n int) string {
	if allAgents {
		return "All agents"
	}
	switch n {
	case 0:
		return "No agents"
	case 1:
		return "1 agent"
	default:
		return strconv.Itoa(n) + " agents"
	}
}

// roleAgentsFromForm maps submitted agent ids to role-agent grants, dropping any
// empty value.
func roleAgentsFromForm(ids []string) []RoleAgent {
	out := make([]RoleAgent, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			out = append(out, RoleAgent{AgentID: id})
		}
	}
	return out
}

// memberViews maps members to Members-table rows with their role badges and the
// current role-id set the assign form pre-checks.
func memberViews(members []Member) []views.MemberView {
	out := make([]views.MemberView, 0, len(members))
	for _, m := range members {
		badges := make([]views.RoleBadge, 0, len(m.Roles))
		ids := make([]string, 0, len(m.Roles))
		for _, role := range m.Roles {
			badges = append(badges, views.RoleBadge{Name: role.Name, Builtin: role.Builtin})
			ids = append(ids, role.ID)
		}
		out = append(out, views.MemberView{
			ID:          m.ID,
			Email:       m.Email,
			DisplayName: m.DisplayName,
			Active:      m.Active,
			RoleBadges:  badges,
			RoleIDs:     ids,
		})
	}
	return out
}

// agentListRows maps agents to the catalog list rows.
func agentListRows(agents []Agent, now time.Time) []views.AgentListRow {
	out := make([]views.AgentListRow, 0, len(agents))
	for _, a := range agents {
		out = append(out, views.AgentListRow{
			ID:            a.ID,
			Name:          a.Name,
			Description:   a.Description,
			LatestVersion: versionText(a.LatestVersion),
			Created:       relativeTime(now, a.CreatedAt),
		})
	}
	return out
}

// agentVersionRows maps an agent's versions to detail-table rows.
func agentVersionRows(versions []AgentVersion, now time.Time) []views.AgentVersionRow {
	out := make([]views.AgentVersionRow, 0, len(versions))
	for _, v := range versions {
		out = append(out, views.AgentVersionRow{
			Version: strconv.Itoa(v.Version),
			Digest:  v.ManifestDigest,
			Notes:   v.Notes,
			Created: relativeTime(now, v.CreatedAt),
		})
	}
	return out
}

// entitlementRows maps agent entitlements to detail-table rows. The principal id
// is carried verbatim (rendered as an ID chip); there is no directory lookup to
// resolve it to a name here.
func entitlementRows(ents []Entitlement) []views.EntitlementRow {
	out := make([]views.EntitlementRow, 0, len(ents))
	for _, e := range ents {
		out = append(out, views.EntitlementRow{
			PrincipalType: e.PrincipalType,
			PrincipalID:   e.PrincipalID,
			MaxFanOut:     fanOutText(e.MaxFanOut),
		})
	}
	return out
}

// agentRoleAccess returns the roles that grant access to agentID — those with
// all-agents access or an explicit grant for the agent.
func agentRoleAccess(roles []Role, agentID string) []views.RoleBadge {
	var out []views.RoleBadge
	for _, role := range roles {
		grants := role.AllAgents
		if !grants {
			for _, g := range role.Agents {
				if g.AgentID == agentID {
					grants = true
					break
				}
			}
		}
		if grants {
			out = append(out, views.RoleBadge{Name: role.Name, Builtin: role.Builtin})
		}
	}
	return out
}

// roleOptions maps roles to selectable checklist options (id for the form value,
// name for the label).
func roleOptions(roles []Role) []views.RoleOption {
	out := make([]views.RoleOption, 0, len(roles))
	for _, r := range roles {
		out = append(out, views.RoleOption{ID: r.ID, Name: r.Name})
	}
	return out
}

// policyPermittedRoles resolves a policy's permitted-role ids to display badges,
// looking each id up in the roles catalog for its name and built-in flag. A
// stale id, or one the caller could not resolve (no roles.manage), degrades to a
// short id label rather than a raw full UUID.
func policyPermittedRoles(roles []Role, ids []string) []views.RoleBadge {
	byID := make(map[string]Role, len(roles))
	for _, r := range roles {
		byID[r.ID] = r
	}
	out := make([]views.RoleBadge, 0, len(ids))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			out = append(out, views.RoleBadge{Name: r.Name, Builtin: r.Builtin})
		} else {
			out = append(out, views.RoleBadge{Name: shortIDLabel(id)})
		}
	}
	return out
}

// shortIDLabel truncates an unresolved id for display, keeping the label from
// becoming a full raw UUID.
func shortIDLabel(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// policyListRows maps policies to the catalog list rows.
func policyListRows(policies []Policy, now time.Time) []views.PolicyListRow {
	out := make([]views.PolicyListRow, 0, len(policies))
	for _, p := range policies {
		out = append(out, views.PolicyListRow{
			ID:             p.ID,
			Name:           p.Name,
			Description:    p.Description,
			CurrentVersion: strconv.Itoa(p.CurrentVersion),
			Created:        relativeTime(now, p.CreatedAt),
		})
	}
	return out
}

// policyVersionRows maps a policy's versions to detail-table rows, surfacing the
// digest and signing provenance.
func policyVersionRows(versions []PolicyVersion, now time.Time) []views.PolicyVersionRow {
	out := make([]views.PolicyVersionRow, 0, len(versions))
	for _, v := range versions {
		out = append(out, views.PolicyVersionRow{
			Version:      strconv.Itoa(v.Version),
			Digest:       v.Digest,
			Algorithm:    v.Signing.Algorithm,
			KeyID:        v.Signing.KeyID,
			AllowedHosts: boundaryAllowlistHosts(v.Boundary.Rules),
			Created:      relativeTime(now, v.CreatedAt),
		})
	}
	return out
}

// boundaryAllowlistHosts reads the pre-authorized egress hosts out of a policy
// boundary's free-form rules. It is defensive: the key may be absent, and the
// value decodes from JSON as a []any of strings, so anything else is ignored.
func boundaryAllowlistHosts(rules map[string]any) []string {
	raw, ok := rules["network_allowlist"]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if h, ok := it.(string); ok {
			if h = strings.TrimSpace(h); h != "" {
				out = append(out, h)
			}
		}
	}
	return out
}

// toAnySlice widens a []string to the []any a free-form rules map holds, so the
// saved allowlist round-trips through JSON as an array (matching how the engine
// reads boundary.rules["network_allowlist"]).
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}

// versionText renders an optional version number, em dash when absent.
func versionText(v *int) string {
	if v == nil {
		return "—"
	}
	return strconv.Itoa(*v)
}

// fanOutText renders an optional fan-out ceiling, em dash when absent.
func fanOutText(v *int) string {
	if v == nil {
		return "—"
	}
	return strconv.Itoa(*v)
}
