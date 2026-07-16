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
			{ID: "r-owner", Name: "Owner", Builtin: true, PermissionCount: "9", AllAgents: true, AccessSummary: "All agents"},
			{ID: "r-deploy", Name: "Deployer", Builtin: false, PermissionCount: "1", PermissionKeys: []string{"agents.run"}, AgentIDs: []string{"a1"}, AccessSummary: "1 agent"},
		},
	}))
	mustContain(t, body,
		"New role", "Owner", "Deployer", "Built-in", "Custom", "All agents", "1 agent",
		"Planner", // agent option in the forms
		`data-uk-modal-open="role-edit-r-deploy"`, `data-uk-modal-open="role-delete-r-deploy"`,
		"checked", // the edit form pre-checks the custom role's held permission/agent
	)
	// The built-in role gets no edit/delete controls.
	if strings.Contains(body, `data-uk-modal-open="role-edit-r-owner"`) {
		t.Errorf("built-in role must be read-only")
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

func TestRenderTeamsReadOnlyNotice(t *testing.T) {
	body := render(t, Teams(TeamsData{Chrome: sampleChrome("teams"), Available: false}))
	mustContain(t, body, "Teams", "Team management", "available yet")
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
		`data-uk-copy="sha256:abc123def"`, "Owner", "Back to agents", "Edit")

	// A caller without agents.manage gets no Edit affordance, and the
	// role-access note replaces the list when roles.manage is absent.
	note := render(t, AgentDetail(AgentDetailData{
		Chrome: sampleChrome("agents"), ID: "a1", Name: "Planner",
		AccessNote: "Viewing which roles grant this agent requires the roles.manage permission.",
	}))
	mustContain(t, note, "roles.manage")
	if strings.Contains(note, ">Edit</button>") {
		t.Errorf("Edit affordance must be hidden without agents.manage")
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
