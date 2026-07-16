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

// This file wires the RBAC governance pages (Roles, Members, Teams) and the
// read-only Agents/Policies detail views onto the control-plane client
// (ADR-0006 §7). Management actions are permission-gated from the caller's
// effective permissions (GET /auth/whoami) so the UI never offers an action the
// server would reject — the server still enforces; this is the UX layer.

// The permission verbs this package gates management actions on. They mirror the
// application catalog (ADR-0006 §2, internal/store/permissions.go); the admin UI
// is a separate binary and never imports the control-plane store, so the small
// set it needs is restated here.
const (
	permRolesManage   = "roles.manage"
	permMembersManage = "members.manage"
	permAuditView     = "audit.view"
	permAgentsManage  = "agents.manage"
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

// --- Teams -----------------------------------------------------------------

// handleTeams renders the Teams page. The control plane exposes no team
// endpoints in this batch, so the page is read-only and explains as much.
func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, _, err := s.buildChrome(r.Context(), token, "teams", "Teams")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	s.render(w, r, http.StatusOK, views.Teams(views.TeamsData{Chrome: chrome, Available: false}))
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

// --- Policies (list + read-only detail) ------------------------------------

// handlePolicies renders the policy catalog list, the entry point into the
// read-only policy detail views.
func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	chrome, _, err := s.buildChrome(r.Context(), token, "policies", "Policies")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.PoliciesData{Chrome: chrome}
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

// handlePolicyDetail renders the read-only detail view for one policy and its
// immutable signed versions.
func (s *Server) handlePolicyDetail(w http.ResponseWriter, r *http.Request) {
	token := tokenFromContext(r.Context())
	policyID := chi.URLParam(r, "policyId")
	chrome, _, err := s.buildChrome(r.Context(), token, "policies", "Policy")
	if err != nil {
		s.redirectToLogin(w, r)
		return
	}
	data := views.PolicyDetailData{Chrome: chrome, ID: policyID}

	policy, err := s.client.GetPolicy(r.Context(), token, policyID)
	switch {
	case errors.Is(err, ErrUnauthorized):
		s.redirectToLogin(w, r)
		return
	case err != nil:
		s.logger.ErrorContext(r.Context(), "admin-ui: get policy failed", slog.Any("error", err))
		data.Error = "Could not load policy: " + readableError(err)
		s.render(w, r, http.StatusOK, views.PolicyDetail(data))
		return
	}

	now := s.now()
	data.Name = policy.Name
	data.Description = policy.Description
	data.CurrentVersion = strconv.Itoa(policy.CurrentVersion)
	data.Created = relativeTime(now, policy.CreatedAt)

	if versions, err := s.client.ListPolicyVersions(r.Context(), token, policyID); err != nil {
		s.logBestEffort(r.Context(), "admin-ui: list policy versions failed", err)
	} else {
		data.Versions = policyVersionRows(versions.Items, now)
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
			Version:   strconv.Itoa(v.Version),
			Digest:    v.Digest,
			Algorithm: v.Signing.Algorithm,
			KeyID:     v.Signing.KeyID,
			Created:   relativeTime(now, v.CreatedAt),
		})
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
