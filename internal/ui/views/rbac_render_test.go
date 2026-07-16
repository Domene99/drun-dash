package views

import (
	"strings"
	"testing"
)

// These hermetic tests render the RBAC governance pages and the read-only
// detail views against fully-resolved view models, asserting the permission
// gating, the built-in-vs-custom controls, form prefill, and the read-only
// notices — complementing the handler tests in internal/ui which drive the same
// views against a mock control plane.

func TestRenderRolesBuiltinReadOnlyAndPrefill(t *testing.T) {
	body := render(t, Roles(RolesData{
		Chrome:      sampleChrome("roles"),
		CanManage:   true,
		Permissions: []PermissionOption{{Key: "agents.run", Description: "run"}, {Key: "roles.manage", Description: "roles"}},
		Agents:      []AgentOption{{ID: "a1", Name: "Planner"}},
		Roles: []RoleView{
			{ID: "r-owner", Name: "Owner", Builtin: true, PermissionCount: "9", PermissionKeys: []string{"roles.manage"}, AllAgents: true, AccessSummary: "All agents"},
			{ID: "r-deploy", Name: "Deployer", Builtin: false, PermissionCount: "1", PermissionKeys: []string{"agents.run"}, AgentIDs: []string{"a1"}, AccessSummary: "1 agent"},
		},
	}))
	mustContain(t, body,
		"New role", "Owner", "Deployer", "Built-in", "Custom", "All agents", "1 agent",
		"Planner", // agent option in the forms
		`data-uk-modal-open="role-edit-r-deploy"`, `data-uk-modal-open="role-delete-r-deploy"`,
		"checked", // the edit form pre-checks the custom role's held permission/agent
		// The built-in role is viewable read-only (see what it grants).
		`data-uk-modal-open="role-view-r-owner"`, "View permissions", "Permissions — Owner",
	)
	// The built-in role gets no edit/delete controls, but IS viewable read-only.
	if strings.Contains(body, `data-uk-modal-open="role-edit-r-owner"`) || strings.Contains(body, `data-uk-modal-open="role-delete-r-owner"`) {
		t.Errorf("built-in role must not be editable")
	}
	// The built-in read-only view renders its granted permissions as disabled
	// (read-only) checkboxes.
	if !strings.Contains(body, "disabled") {
		t.Errorf("built-in role view must render read-only (disabled) permission checkboxes:\n%s", body)
	}
}

func TestRenderRolesAllAgentsGraysAndChecksList(t *testing.T) {
	body := render(t, Roles(RolesData{
		Chrome:      sampleChrome("roles"),
		CanManage:   true,
		Permissions: []PermissionOption{{Key: "agents.run", Description: "run"}},
		Agents:      []AgentOption{{ID: "a1", Name: "Planner"}, {ID: "a2", Name: "Reviewer"}},
		Roles: []RoleView{
			// A custom all-agents role: its per-agent list is checked + greyed.
			{ID: "r-all", Name: "All", Builtin: false, PermissionCount: "1", AllAgents: true, AccessSummary: "All agents"},
		},
	}))
	mustContain(t, body,
		// The all-agents toggle wires the per-agent list for the live script.
		`data-all-agents-toggle`, `data-agent-list="role-edit-r-all-agent-list"`,
		`id="role-edit-r-all-agent-list"`,
		// Server-rendered greyed container (correct even with JS off).
		"uk-agent-checklist-disabled",
		// The tiny inline vanilla toggle script (no framework).
		"addEventListener", "querySelectorAll",
	)
	// With all-agents on, each per-agent checkbox is server-rendered checked+disabled.
	if !strings.Contains(body, "checked") || !strings.Contains(body, "disabled") {
		t.Errorf("all-agents role must render its per-agent list checked and disabled:\n%s", body)
	}
}

func TestRenderRolesViewOnlyNotice(t *testing.T) {
	body := render(t, Roles(RolesData{Chrome: sampleChrome("roles"), NoAccess: true}))
	mustContain(t, body, "View-only")
	if strings.Contains(body, "New role") {
		t.Errorf("view-only roles page must not offer New role")
	}
}

func TestRenderMembersBadgesAndGating(t *testing.T) {
	data := MembersData{
		Chrome:    sampleChrome("members"),
		CanManage: true,
		Roles:     []RoleOption{{ID: "r1", Name: "Owner"}},
		Members: []MemberView{
			{ID: "m1", Email: "dev@example.com", DisplayName: "Dev One", Active: true,
				RoleBadges: []RoleBadge{{Name: "Owner", Builtin: true}}, RoleIDs: []string{"r1"}},
			{ID: "m2", Email: "aud@example.com", Active: false},
		},
	}
	body := render(t, Members(data))
	mustContain(t, body, "Dev One", "dev@example.com", "Owner", "Active", "Inactive",
		"Assign roles", `data-uk-modal-open="member-roles-m1"`, "checked")

	// Without members.manage the assign action disappears.
	viewOnly := render(t, Members(MembersData{Chrome: sampleChrome("members"), Members: data.Members}))
	if strings.Contains(viewOnly, "Assign roles") {
		t.Errorf("assign action must be hidden without members.manage")
	}
}

func TestRenderAgentDetailReadOnly(t *testing.T) {
	body := render(t, AgentDetail(AgentDetailData{
		Chrome: sampleChrome("agents"), ID: "a1", Name: "Planner", Description: "plans",
		LatestVersion: "2", DefaultPolicy: "Production", DefaultPolicyID: "p1",
		Created:   TimeView{Relative: "1 day ago"},
		Versions:  []AgentVersionRow{{Version: "2", Digest: "sha256:abc123def", Notes: "n", Created: TimeView{Relative: "now"}}},
		Access:    []RoleBadge{{Name: "Owner", Builtin: true}},
		CanManage: true,
	}))
	mustContain(t, body, "Planner", "plans", "Production", `href="/policies/p1"`,
		`data-uk-copy="sha256:abc123def"`, "Owner", "Back to agents",
		// The Edit affordance links to the edit-agent form (agents.manage).
		"Edit", `href="/agents/a1/edit"`)

	// A caller without agents.manage gets no Edit affordance, and the
	// role-access note replaces the list when roles.manage is absent.
	note := render(t, AgentDetail(AgentDetailData{
		Chrome: sampleChrome("agents"), ID: "a1", Name: "Planner",
		AccessNote: "Viewing which roles grant this agent requires the roles.manage permission.",
	}))
	mustContain(t, note, "roles.manage")
	if strings.Contains(note, `href="/agents/a1/edit"`) {
		t.Errorf("Edit affordance must be hidden without agents.manage")
	}
}

func TestRenderAgentEditForm(t *testing.T) {
	body := render(t, AgentEdit(AgentEditData{
		Chrome: sampleChrome("agents"), ID: "a1", Name: "Planner", Description: "plans",
		Policies:   []PolicyOption{{ID: "p1", Name: "Prod"}},
		Endpoints:  []EndpointOption{{ID: "e1", Name: "Anthropic prod"}},
		PolicyID:   "p1",
		EndpointID: "e1",
	}))
	mustContain(t, body,
		"Edit agent", `action="/agents/a1/edit"`, "Save changes",
		`name="name"`, "Planner", `name="description"`, `name="system_prompt"`,
		`name="default_policy_id"`, `name="endpoint_id"`, "Prod", "Anthropic prod",
		// The policy governs the boundary; the form says so and drops tools/fan-out.
		"Tools, network, and concurrency are governed by the selected policy.",
	)
	if !strings.Contains(body, `value="p1" selected`) || !strings.Contains(body, `value="e1" selected`) {
		t.Errorf("edit form must preselect the agent's policy and endpoint:\n%s", body)
	}
	if strings.Contains(body, `name="allowed_tools"`) || strings.Contains(body, `name="fan_out_ceiling"`) {
		t.Errorf("edit form must not collect tools or fan-out:\n%s", body)
	}
}

func TestRenderAgentEditNoAccess(t *testing.T) {
	body := render(t, AgentEdit(AgentEditData{Chrome: sampleChrome("agents"), ID: "a1", NoAccess: true}))
	mustContain(t, body, "No access", "agents.manage")
	if strings.Contains(body, `name="system_prompt"`) {
		t.Errorf("edit form must be hidden without agents.manage")
	}
}

func TestRenderPolicyDetailVersions(t *testing.T) {
	body := render(t, PolicyDetail(PolicyDetailData{
		Chrome: sampleChrome("policies"), ID: "p1", Name: "Production", Description: "prod",
		CurrentVersion: "3", Created: TimeView{Relative: "now"},
		Versions: []PolicyVersionRow{{Version: "3", Digest: "sha256:deadbeef", Algorithm: "ed25519", KeyID: "key-1", Created: TimeView{Relative: "now"}}},
	}))
	mustContain(t, body, "Production", "prod", "ed25519", "key-1",
		`data-uk-copy="sha256:deadbeef"`, "Back to policies")
}

// TestRenderPolicyDetailPermittedRoles asserts the policy detail page renders the
// permitted-role badges, the policyctl note, and (only with CanManage) the edit
// affordance with the current selection pre-checked.
func TestRenderPolicyDetailPermittedRoles(t *testing.T) {
	body := render(t, PolicyDetail(PolicyDetailData{
		Chrome: sampleChrome("policies"), ID: "p1", Name: "Production",
		CurrentVersion: "1", Created: TimeView{Relative: "1 day ago"},
		CanManage:        true,
		PermittedRoles:   []RoleBadge{{Name: "Deployer"}},
		AllRoles:         []RoleOption{{ID: "r-deploy", Name: "Deployer"}, {ID: "r-owner", Name: "Owner"}},
		PermittedRoleIDs: []string{"r-deploy"},
	}))
	mustContain(t, body, "Permitted roles", "Deployer", "Edit permitted roles",
		`data-uk-modal-open="policy-roles-p1"`, "policyctl", "checked")

	ro := render(t, PolicyDetail(PolicyDetailData{
		Chrome: sampleChrome("policies"), ID: "p1", Name: "Production",
		CurrentVersion: "1", Created: TimeView{Relative: "now"},
	}))
	if strings.Contains(ro, "Edit permitted roles") {
		t.Errorf("edit affordance must be hidden without CanManage")
	}
	if !strings.Contains(ro, "No role is granted by this policy") {
		t.Errorf("empty permitted-roles set should render the no-role note")
	}
}

// TestRenderPoliciesCreateModalHasPermittedRoles asserts the New-policy dialog
// offers the permitted-roles checklist built from the org's roles.
func TestRenderPoliciesCreateModalHasPermittedRoles(t *testing.T) {
	body := render(t, Policies(PoliciesData{
		Chrome: sampleChrome("policies"), CanManage: true,
		Roles: []RoleOption{{ID: "r-deploy", Name: "Deployer"}},
	}))
	mustContain(t, body, "New policy", "Permitted roles", "Deployer",
		`name="permitted_roles"`, `value="r-deploy"`)
}
