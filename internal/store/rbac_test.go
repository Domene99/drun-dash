package store

import (
	"testing"
)

// These tests cover the pure RBAC logic that needs no live database: the
// permission catalog, the built-in role definitions, and the grant-folding /
// validation helpers. Database-backed coverage lives in rbac_integration_test.go
// behind the `integration` build tag.

func TestAllPermissionsAndValidity(t *testing.T) {
	all := AllPermissions()
	if len(all) != 9 {
		t.Fatalf("AllPermissions len = %d, want 9", len(all))
	}
	for _, p := range all {
		if !IsValidPermission(p) {
			t.Errorf("IsValidPermission(%q) = false, want true", p)
		}
	}
	// The returned slice is a copy: mutating it must not affect the catalog.
	all[0] = "mutated"
	if AllPermissions()[0] == "mutated" {
		t.Fatal("AllPermissions returned a slice aliasing the package catalog")
	}
	for _, p := range []Permission{"", "agents.MANAGE", "agents.launch", "owner", "audit"} {
		if IsValidPermission(p) {
			t.Errorf("IsValidPermission(%q) = true, want false", p)
		}
	}
}

func TestBuiltinRolesShape(t *testing.T) {
	byName := map[string]BuiltinRole{}
	for _, b := range BuiltinRoles {
		byName[b.Name] = b
	}
	if len(byName) != 4 {
		t.Fatalf("BuiltinRoles has %d distinct names, want 4", len(byName))
	}

	// Owner: every permission + all agents.
	owner := byName[BuiltinOwner]
	if !owner.AllAgents {
		t.Error("Owner.AllAgents = false, want true")
	}
	if len(owner.Permissions) != len(AllPermissions()) {
		t.Errorf("Owner has %d permissions, want every permission (%d)", len(owner.Permissions), len(AllPermissions()))
	}

	// Admin: all *.manage + approvals.decide + audit.view, but NOT agents.run.
	admin := byName[BuiltinAdmin]
	if admin.AllAgents {
		t.Error("Admin.AllAgents = true, want false")
	}
	if hasPerm(admin.Permissions, PermAgentsRun) {
		t.Error("Admin must not hold agents.run")
	}
	for _, want := range []Permission{
		PermAgentsManage, PermPoliciesManage, PermEndpointsManage, PermBudgetsManage,
		PermMembersManage, PermRolesManage, PermApprovalsDecide, PermAuditView,
	} {
		if !hasPerm(admin.Permissions, want) {
			t.Errorf("Admin missing %q", want)
		}
	}

	// Auditor: audit.view only.
	auditor := byName[BuiltinAuditor]
	if len(auditor.Permissions) != 1 || auditor.Permissions[0] != PermAuditView {
		t.Errorf("Auditor permissions = %v, want [audit.view]", auditor.Permissions)
	}

	// Member: agents.run only, no all-agents by default.
	member := byName[BuiltinMember]
	if member.AllAgents {
		t.Error("Member.AllAgents = true, want false")
	}
	if len(member.Permissions) != 1 || member.Permissions[0] != PermAgentsRun {
		t.Errorf("Member permissions = %v, want [agents.run]", member.Permissions)
	}
}

func TestValidatePermissions(t *testing.T) {
	// Dedupes and returns in catalog order.
	got, err := validatePermissions([]Permission{PermAgentsRun, PermAuditView, PermAgentsRun, PermAgentsManage})
	if err != nil {
		t.Fatalf("validatePermissions: %v", err)
	}
	want := []Permission{PermAgentsManage, PermAuditView, PermAgentsRun}
	if len(got) != len(want) {
		t.Fatalf("validatePermissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("validatePermissions = %v, want %v (catalog order)", got, want)
		}
	}
	// Unknown permission is rejected.
	if _, err := validatePermissions([]Permission{PermAgentsRun, "bogus.perm"}); err == nil {
		t.Fatal("validatePermissions(bogus) = nil error, want ErrInvalidPermission")
	}
	// Empty input is valid and yields no permissions.
	if got, err := validatePermissions(nil); err != nil || len(got) != 0 {
		t.Fatalf("validatePermissions(nil) = %v, %v; want [], nil", got, err)
	}
}

func TestFilterValidPermissions(t *testing.T) {
	in := []Permission{PermAgentsRun, "unknown", PermAuditView}
	got := filterValidPermissions(in)
	if len(got) != 2 || got[0] != PermAgentsRun || got[1] != PermAuditView {
		t.Fatalf("filterValidPermissions = %v, want [agents.run audit.view]", got)
	}
}

func TestDedupeGrants(t *testing.T) {
	fanA, fanB := 2, 5
	grants := []RoleAgentGrant{
		{AgentID: "a", MaxFanOut: &fanA},
		{AgentID: "b"},
		{AgentID: "a", MaxFanOut: &fanB}, // duplicate agent id, later wins
	}
	got := dedupeGrants(grants)
	if len(got) != 2 {
		t.Fatalf("dedupeGrants len = %d, want 2", len(got))
	}
	if got[0].AgentID != "a" || got[0].MaxFanOut == nil || *got[0].MaxFanOut != fanB {
		t.Fatalf("dedupeGrants kept wrong grant for a: %+v", got[0])
	}
	if got[1].AgentID != "b" {
		t.Fatalf("dedupeGrants order/agent wrong: %+v", got)
	}
	if dedupeGrants(nil) != nil {
		t.Fatal("dedupeGrants(nil) != nil")
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"r1", "r2", "r1", "r3", "r2"})
	want := []string{"r1", "r2", "r3"}
	if len(got) != len(want) {
		t.Fatalf("dedupeStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeStrings = %v, want %v (first-appearance order)", got, want)
		}
	}
	if dedupeStrings(nil) != nil {
		t.Fatal("dedupeStrings(nil) != nil")
	}
}

func hasPerm(perms []Permission, p Permission) bool {
	for _, x := range perms {
		if x == p {
			return true
		}
	}
	return false
}
