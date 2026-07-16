package views

// This file holds the view models and small presentation helpers for the RBAC
// governance pages (Roles, Members, Teams) and the read-only Agents/Policies
// detail views (ADR-0006). Like the rest of the package they are pure
// presentation: internal/ui assembles them from control-plane responses and the
// templ views render them. Raw UUIDs only ever appear inside an ID chip, never
// as a primary label (ADR-0005 §4).

// PermissionOption is one entry in the role-editor permission checklist: the
// permission verb and a human description of what it governs.
type PermissionOption struct {
	Key         string
	Description string
}

// AgentOption is one selectable agent in the role-editor agent-access checklist.
// Name is the primary label; ID is carried for the form value only.
type AgentOption struct {
	ID   string
	Name string
}

// RoleOption is one selectable role in the member role-assignment control.
type RoleOption struct {
	ID   string
	Name string
}

// RoleBadge is a role shown as a pill next to a member (or team): the role name
// plus whether it is a built-in role (rendered with an accent badge).
type RoleBadge struct {
	Name    string
	Builtin bool
}

// RolesData is the view model for the Roles page (/roles). CanManage gates the
// create/edit/delete controls (roles.manage); Permissions and Agents feed the
// create/edit forms. Error carries a page-level message (a load or write
// failure); NoAccess renders the permission-gated notice instead of the table.
type RolesData struct {
	Chrome      Chrome
	Error       string
	NoAccess    bool
	CanManage   bool
	Roles       []RoleView
	Permissions []PermissionOption
	Agents      []AgentOption
}

// RoleView is one row of the Roles table plus the state its edit form needs.
// PermissionKeys / AgentIDs preselect the checklists; AccessSummary is the
// human agent-access summary ("All agents", "3 agents", "No agents").
type RoleView struct {
	ID              string
	Name            string
	Description     string
	Builtin         bool
	PermissionCount string
	PermissionKeys  []string
	AllAgents       bool
	AgentIDs        []string
	AccessSummary   string
	Created         TimeView
}

// MembersData is the view model for the Members page (/members). CanManage gates
// the assign-roles control (members.manage); Roles is the assignable-role list
// for the multiselect. NoAccess renders the permission notice instead of the
// table.
type MembersData struct {
	Chrome    Chrome
	Error     string
	NoAccess  bool
	CanManage bool
	Members   []MemberView
	Roles     []RoleOption
	// InviteLink, when set, holds the accept-invite link surfaced after a
	// successful "Invite member" so the admin can copy and hand it off (no
	// mailer in local dev); InviteEmail names who it is for.
	InviteLink  string
	InviteEmail string
}

// MemberView is one row of the Members table plus the state its assign-roles
// form needs. RoleBadges renders the current roles; RoleIDs preselects the
// multiselect.
type MemberView struct {
	ID          string
	Email       string
	DisplayName string
	Active      bool
	RoleBadges  []RoleBadge
	RoleIDs     []string
}

// TeamsData is the view model for the Teams page (/teams). The control plane
// exposes no team-management endpoints in this batch, so the page is read-only:
// Available is false and the view renders an explanatory notice (ADR-0006).
type TeamsData struct {
	Chrome    Chrome
	Error     string
	Available bool
	Teams     []TeamView
}

// TeamView is one row of the Teams table (name, member count, roles), used when
// the API exposes teams. It is defined now so enabling a teams endpoint later is
// a wiring change, not a view rewrite.
type TeamView struct {
	ID          string
	Name        string
	MemberCount string
	RoleBadges  []RoleBadge
}

// AgentsData is the view model for the Agents catalog list (/agents): the entry
// point into the read-only agent detail views.
type AgentsData struct {
	Chrome Chrome
	Error  string
	Agents []AgentListRow
	// CanManage gates the "New agent" header action (agents.manage).
	CanManage bool
}

// AgentListRow is one row of the Agents catalog table.
type AgentListRow struct {
	ID            string
	Name          string
	Description   string
	LatestVersion string
	Created       TimeView
}

// AgentDetailData is the view model for the read-only agent detail view
// (/agents/{id}). DefaultPolicy is resolved to the policy NAME where possible
// (its id kept for the chip); Access lists the roles that grant this agent.
// CanManage gates the (next-batch) Edit affordance; AccessNote explains when the
// role-access list could not be resolved (the caller lacks roles.manage).
type AgentDetailData struct {
	Chrome          Chrome
	Error           string
	ID              string
	Name            string
	Description     string
	LatestVersion   string
	DefaultPolicy   string
	DefaultPolicyID string
	Created         TimeView
	Updated         TimeView
	Versions        []AgentVersionRow
	Access          []RoleBadge
	AccessNote      string
	Entitlements    []EntitlementRow
	CanManage       bool
}

// AgentVersionRow is one row of an agent's version history. Digest is the
// manifest content digest (shown as an ID chip).
type AgentVersionRow struct {
	Version string
	Digest  string
	Notes   string
	Created TimeView
}

// EntitlementRow is one principal entitled to launch an agent: the principal
// kind (user/group) as a badge and its id as an ID chip (no name to resolve to
// without a directory lookup, so the raw id stays inside a chip per ADR-0005).
type EntitlementRow struct {
	PrincipalType string
	PrincipalID   string
	MaxFanOut     string
}

// PoliciesData is the view model for the Policies list (/policies): the entry
// point into the read-only policy detail views.
type PoliciesData struct {
	Chrome   Chrome
	Error    string
	Policies []PolicyListRow
}

// PolicyListRow is one row of the Policies table.
type PolicyListRow struct {
	ID             string
	Name           string
	Description    string
	CurrentVersion string
	Created        TimeView
}

// PolicyDetailData is the view model for the read-only policy detail view
// (/policies/{id}): the policy plus its immutable signed versions.
type PolicyDetailData struct {
	Chrome         Chrome
	Error          string
	ID             string
	Name           string
	Description    string
	CurrentVersion string
	Created        TimeView
	Versions       []PolicyVersionRow
}

// PolicyVersionRow is one row of a policy's version history. Digest is shown as
// an ID chip; Algorithm/KeyID summarise the signing provenance.
type PolicyVersionRow struct {
	Version   string
	Digest    string
	Algorithm string
	KeyID     string
	Created   TimeView
}

// rolePath is the roles page URL.
const rolePath = "/roles"

// roleUpdatePath is the update-form POST URL for a role id.
func roleUpdatePath(roleID string) string {
	return "/roles/" + roleID + "/update"
}

// roleDeletePath is the delete POST URL for a role id.
func roleDeletePath(roleID string) string {
	return "/roles/" + roleID + "/delete"
}

// memberRolesPath is the assign-roles POST URL for a member id.
func memberRolesPath(memberID string) string {
	return "/members/" + memberID + "/roles"
}

// agentDetailPath is the detail-view URL for an agent id.
func agentDetailPath(agentID string) string {
	return "/agents/" + agentID
}

// policyDetailPath is the detail-view URL for a policy id.
func policyDetailPath(policyID string) string {
	return "/policies/" + policyID
}

// roleEditModalID / roleDeleteModalID derive the per-row modal element ids so a
// row's Edit/Delete controls open the matching dialog.
func roleEditModalID(roleID string) string   { return "role-edit-" + roleID }
func roleDeleteModalID(roleID string) string { return "role-delete-" + roleID }

// memberRolesModalID derives the per-row assign-roles modal element id.
func memberRolesModalID(memberID string) string { return "member-roles-" + memberID }

// contains reports whether s is present in list, for preselecting a checklist
// or multiselect option in the role/member editors.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// memberLabel is the human label for a member: their display name when set,
// otherwise their email. Used in dialog titles.
func memberLabel(m MemberView) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Email
}
