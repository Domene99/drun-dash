package views

// This file holds the view models and small presentation helpers for the RBAC
// governance pages (Roles, Members) and the read-only Agents/Policies
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
// point into the read-only policy detail views. CanManage gates the "New policy"
// header action and its create dialog (policies.manage).
type PoliciesData struct {
	Chrome    Chrome
	Error     string
	CanManage bool
	Policies  []PolicyListRow
	// Roles is the org's roles, offered as the create dialog's permitted-roles
	// checklist (populated only for a manager who may create policies).
	Roles []RoleOption
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
	// CanManage gates the "Edit permitted roles" affordance (policies.manage).
	CanManage bool
	// PermittedRoles are the roles this policy grants (resolved ids->names) for
	// display; AllRoles is the full catalog and PermittedRoleIDs the current
	// selection, both feeding the edit checklist.
	PermittedRoles   []RoleBadge
	AllRoles         []RoleOption
	PermittedRoleIDs []string
}

// PolicyVersionRow is one row of a policy's version history. Digest is shown as
// an ID chip; Algorithm/KeyID summarise the signing provenance.
type PolicyVersionRow struct {
	Version   string
	Digest    string
	Algorithm string
	KeyID     string
	// AllowedHosts are the pre-authorized egress hosts from the version's
	// boundary (rules.network_allowlist), shown read-only; empty when none.
	AllowedHosts []string
	Created      TimeView
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

// agentEditPath is the edit-agent form URL for an agent id.
func agentEditPath(agentID string) string {
	return "/agents/" + agentID + "/edit"
}

// policyDetailPath is the detail-view URL for a policy id.
func policyDetailPath(policyID string) string {
	return "/policies/" + policyID
}

// policyRolesPath is the edit-permitted-roles POST URL for a policy id.
func policyRolesPath(policyID string) string {
	return "/policies/" + policyID + "/roles"
}

// policyRolesModalID derives the per-policy edit-permitted-roles modal element
// id so the detail page's Edit control opens the matching dialog.
func policyRolesModalID(policyID string) string { return "policy-roles-" + policyID }

// roleEditModalID / roleDeleteModalID / roleViewModalID derive the per-row modal
// element ids so a row's Edit/Delete (custom) or View-permissions (built-in)
// control opens the matching dialog.
func roleEditModalID(roleID string) string   { return "role-edit-" + roleID }
func roleDeleteModalID(roleID string) string { return "role-delete-" + roleID }
func roleViewModalID(roleID string) string   { return "role-view-" + roleID }

// memberRolesModalID derives the per-row assign-roles modal element id.
func memberRolesModalID(memberID string) string { return "member-roles-" + memberID }

// agentListClass is the class for the role form's per-agent checklist container.
// When the role grants all agents the individual checks are moot, so the
// container is greyed (matching the checked+disabled state the toggle script
// keeps in sync live).
func agentListClass(allAgents bool) string {
	if allAgents {
		return "uk-agent-checklist uk-agent-checklist-disabled"
	}
	return "uk-agent-checklist"
}

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

// --- Endpoints -------------------------------------------------------------

// EndpointsData is the view model for the Endpoints page (/endpoints): the org's
// registered model endpoints. CanManage gates the create/edit/delete controls
// (endpoints.manage); without it NoAccess renders a permission notice instead of
// the table. Providers feeds the provider select in the create/edit forms. Error
// carries a page-level message (a load or write failure). The stored API key is
// never part of this model — only SecretSet (whether a key is on file) is.
type EndpointsData struct {
	Chrome    Chrome
	Error     string
	NoAccess  bool
	CanManage bool
	Endpoints []EndpointRow
	Providers []string
}

// EndpointRow is one row of the Endpoints table. SecretSet drives the
// "Key configured"/"No key" badge; the raw key is never rendered. Provider is
// pre-selected in the row's edit dialog.
type EndpointRow struct {
	ID        string
	Name      string
	Provider  string
	BaseURL   string
	Active    bool
	SecretSet bool
	Created   TimeView
}

// EndpointOption is one selectable endpoint in the agent-create form's
// model-endpoint dropdown. Name is the primary label (the app appends the
// provider and a no-key hint); ID is the form value.
type EndpointOption struct {
	ID   string
	Name string
}

// endpointUpdatePath is the update-form POST URL for an endpoint id.
func endpointUpdatePath(id string) string {
	return "/endpoints/" + id + "/update"
}

// endpointDeletePath is the delete POST URL for an endpoint id.
func endpointDeletePath(id string) string {
	return "/endpoints/" + id + "/delete"
}

// endpointEditModalID / endpointDeleteModalID derive the per-row modal element
// ids so a row's Edit/Delete controls open the matching dialog.
func endpointEditModalID(id string) string   { return "endpoint-edit-" + id }
func endpointDeleteModalID(id string) string { return "endpoint-delete-" + id }

// endpointKeyBadge summarises whether an endpoint has a sealed API key on file,
// for the Endpoints table's key column (text + color, never the key itself).
func endpointKeyBadge(secretSet bool) string {
	if secretSet {
		return "Key configured"
	}
	return "No key"
}

// endpointStatus renders an endpoint's active flag as a domain status string the
// shared StatusBadge vocabulary colors.
func endpointStatus(active bool) string {
	if active {
		return "active"
	}
	return "inactive"
}

// endpointKeyHelp is the edit-dialog help for the write-only API key field,
// stating whether a key is currently on file and that leaving it blank keeps it.
func endpointKeyHelp(secretSet bool) string {
	if secretSet {
		return "A key is on file. Leave blank to keep it, or enter a new one to replace it."
	}
	return "No key is on file. Enter one so agents pinned to this endpoint can run."
}
