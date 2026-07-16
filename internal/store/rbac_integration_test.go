//go:build integration

// Integration coverage for the RBAC repositories (roles, role_permissions,
// role_agents, member_roles, team_roles) and the AccessResolver, mirroring the
// other *_integration_test.go files: they share requirePool / newOrg / uniq /
// newMember and skip when Postgres is unreachable. Each test hangs its rows off
// a throwaway org whose cleanup cascades. The policy-centric AuthorizeRun /
// ListAccessibleAgentIDs coverage (ADR-0010) lives in
// policy_roles_integration_test.go alongside the policy_roles store tests.
package store

import (
	"context"
	"errors"
	"testing"
)

// seedBuiltins creates the four built-in roles for orgID via the store (the
// migration seeds pre-existing orgs; test orgs are created afterwards, so they
// seed themselves here). It returns the roles keyed by name.
func seedBuiltins(t *testing.T, ctx context.Context, db Querier, orgID string) map[string]Role {
	t.Helper()
	roles := NewRoleStore(db)
	out := map[string]Role{}
	for _, b := range BuiltinRoles {
		desc := b.Description
		r, err := roles.Create(ctx, CreateRoleParams{
			OrgID: orgID, Name: b.Name, Description: &desc,
			Builtin: true, AllAgents: b.AllAgents, Permissions: b.Permissions,
		})
		if err != nil {
			t.Fatalf("seed builtin %q: %v", b.Name, err)
		}
		out[b.Name] = r
	}
	return out
}

func TestIntegrationRoleStoreCRUD(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent, err := NewAgentStore(pool).Create(ctx, org.ID, "a", nil)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	roles := NewRoleStore(pool)

	desc := "run the summarizer, view audit"
	fan := 3
	created, err := roles.Create(ctx, CreateRoleParams{
		OrgID: org.ID, Name: "Summarizer ops", Description: &desc,
		Permissions: []Permission{PermAgentsRun, PermAuditView},
		Agents:      []RoleAgentGrant{{AgentID: agent.ID, MaxFanOut: &fan}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || created.Builtin || created.AllAgents || created.CreatedAt.IsZero() {
		t.Fatalf("Create returned unexpected row: %+v", created)
	}
	if len(created.Permissions) != 2 || len(created.Agents) != 1 || created.Agents[0].AgentID != agent.ID {
		t.Fatalf("Create did not hydrate perms/agents: %+v", created)
	}

	got, err := roles.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Permissions) != 2 || len(got.Agents) != 1 {
		t.Fatalf("GetByID hydration = %+v", got)
	}
	if got.Agents[0].MaxFanOut == nil || *got.Agents[0].MaxFanOut != fan {
		t.Fatalf("GetByID lost per-grant max_fan_out: %+v", got.Agents[0])
	}

	// Update base fields (rename + all_agents).
	updated, err := roles.Update(ctx, created.ID, UpdateRoleParams{Name: "Summarizer", Description: &desc, AllAgents: true})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "Summarizer" || !updated.AllAgents {
		t.Fatalf("Update did not apply: %+v", updated)
	}

	// SetPermissions replaces wholesale.
	if err := roles.SetPermissions(ctx, created.ID, []Permission{PermAuditView}); err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}
	afterPerms, _ := roles.GetByID(ctx, created.ID)
	if len(afterPerms.Permissions) != 1 || afterPerms.Permissions[0] != PermAuditView {
		t.Fatalf("SetPermissions result = %v", afterPerms.Permissions)
	}
	// Invalid permission is rejected.
	if err := roles.SetPermissions(ctx, created.ID, []Permission{"nope"}); !errors.Is(err, ErrInvalidPermission) {
		t.Fatalf("SetPermissions(bogus) = %v, want ErrInvalidPermission", err)
	}

	// SetAgents replaces wholesale.
	if err := roles.SetAgents(ctx, created.ID, nil); err != nil {
		t.Fatalf("SetAgents(nil): %v", err)
	}
	afterAgents, _ := roles.GetByID(ctx, created.ID)
	if len(afterAgents.Agents) != 0 {
		t.Fatalf("SetAgents(nil) left agents: %+v", afterAgents.Agents)
	}

	// ListByOrg returns hydrated roles ordered by name.
	if _, err := roles.Create(ctx, CreateRoleParams{OrgID: org.ID, Name: "Auditors", Permissions: []Permission{PermAuditView}}); err != nil {
		t.Fatalf("Create second: %v", err)
	}
	list, err := roles.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].Name != "Auditors" || list[1].Name != "Summarizer" {
		t.Fatalf("ListByOrg order = %+v", list)
	}

	// Duplicate (org_id, name) -> ErrConflict; missing id -> ErrNotFound.
	if _, err := roles.Create(ctx, CreateRoleParams{OrgID: org.ID, Name: "Auditors"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create = %v, want ErrConflict", err)
	}
	if _, err := roles.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}

	// Delete a custom role.
	if err := roles.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := roles.Delete(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(absent) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationRoleStoreImmutableBuiltins(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	roles := NewRoleStore(pool)
	owner := builtins[BuiltinOwner]

	// Renaming a builtin is rejected.
	if _, err := roles.Update(ctx, owner.ID, UpdateRoleParams{Name: "Superuser"}); !errors.Is(err, ErrImmutableBuiltin) {
		t.Fatalf("Update(rename builtin) = %v, want ErrImmutableBuiltin", err)
	}
	// Changing a builtin's permissions is rejected.
	if err := roles.SetPermissions(ctx, owner.ID, []Permission{PermAuditView}); !errors.Is(err, ErrImmutableBuiltin) {
		t.Fatalf("SetPermissions(builtin) = %v, want ErrImmutableBuiltin", err)
	}
	// Deleting a builtin is rejected (protects the Owner role).
	if err := roles.Delete(ctx, owner.ID); !errors.Is(err, ErrImmutableBuiltin) {
		t.Fatalf("Delete(builtin) = %v, want ErrImmutableBuiltin", err)
	}
	// A builtin's agent access IS editable, and its all_agents / description too.
	agent, err := NewAgentStore(pool).Create(ctx, org.ID, "a", nil)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	member := builtins[BuiltinMember]
	if err := roles.SetAgents(ctx, member.ID, []RoleAgentGrant{{AgentID: agent.ID}}); err != nil {
		t.Fatalf("SetAgents on builtin Member: %v", err)
	}
	if got, _ := roles.GetByID(ctx, member.ID); len(got.Agents) != 1 {
		t.Fatalf("builtin Member agent access not editable: %+v", got.Agents)
	}
}

func TestIntegrationMemberRoleAssignment(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	member := newMember(t, ctx, pool, org.ID)
	mrs := NewMemberRoleStore(pool)
	admin := builtins[BuiltinAdmin]

	if _, err := mrs.AssignRole(ctx, member.ID, admin.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	// Duplicate assignment -> ErrConflict.
	if _, err := mrs.AssignRole(ctx, member.ID, admin.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate AssignRole = %v, want ErrConflict", err)
	}

	rolesForMember, err := mrs.ListRolesForMember(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListRolesForMember: %v", err)
	}
	if len(rolesForMember) != 1 || rolesForMember[0].ID != admin.ID || len(rolesForMember[0].Permissions) == 0 {
		t.Fatalf("ListRolesForMember = %+v (want hydrated Admin)", rolesForMember)
	}

	membersForRole, err := mrs.ListMembersForRole(ctx, admin.ID)
	if err != nil {
		t.Fatalf("ListMembersForRole: %v", err)
	}
	if len(membersForRole) != 1 || membersForRole[0].ID != member.ID {
		t.Fatalf("ListMembersForRole = %+v", membersForRole)
	}

	if err := mrs.RemoveRole(ctx, member.ID, admin.ID); err != nil {
		t.Fatalf("RemoveRole: %v", err)
	}
	if err := mrs.RemoveRole(ctx, member.ID, admin.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RemoveRole(absent) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationLastOwnerGuard(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	owner := builtins[BuiltinOwner]
	mrs := NewMemberRoleStore(pool)

	m1 := newMember(t, ctx, pool, org.ID)
	m2 := newMember(t, ctx, pool, org.ID)
	if _, err := mrs.AssignRole(ctx, m1.ID, owner.ID); err != nil {
		t.Fatalf("assign owner to m1: %v", err)
	}

	// Sole Owner assignment cannot be removed.
	if err := mrs.RemoveRole(ctx, m1.ID, owner.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("RemoveRole(last owner) = %v, want ErrLastOwner", err)
	}

	// With a second Owner, either may be removed.
	if _, err := mrs.AssignRole(ctx, m2.ID, owner.ID); err != nil {
		t.Fatalf("assign owner to m2: %v", err)
	}
	if err := mrs.RemoveRole(ctx, m1.ID, owner.ID); err != nil {
		t.Fatalf("RemoveRole with two owners: %v", err)
	}
	// Now m2 is again the last Owner.
	if err := mrs.RemoveRole(ctx, m2.ID, owner.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("RemoveRole(new last owner) = %v, want ErrLastOwner", err)
	}
}

func TestIntegrationTeamRoleAssignment(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	auditor := builtins[BuiltinAuditor]
	team, err := NewTeamStore(pool).Create(ctx, org.ID, "platform")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	trs := NewTeamRoleStore(pool)

	if _, err := trs.AssignRole(ctx, team.ID, auditor.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if _, err := trs.AssignRole(ctx, team.ID, auditor.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate AssignRole = %v, want ErrConflict", err)
	}
	rolesForTeam, err := trs.ListRolesForTeam(ctx, team.ID)
	if err != nil {
		t.Fatalf("ListRolesForTeam: %v", err)
	}
	if len(rolesForTeam) != 1 || rolesForTeam[0].ID != auditor.ID {
		t.Fatalf("ListRolesForTeam = %+v", rolesForTeam)
	}
	teamsForRole, err := trs.ListTeamsForRole(ctx, auditor.ID)
	if err != nil {
		t.Fatalf("ListTeamsForRole: %v", err)
	}
	if len(teamsForRole) != 1 || teamsForRole[0].ID != team.ID {
		t.Fatalf("ListTeamsForRole = %+v", teamsForRole)
	}
	if err := trs.RemoveRole(ctx, team.ID, auditor.ID); err != nil {
		t.Fatalf("RemoveRole: %v", err)
	}
	if err := trs.RemoveRole(ctx, team.ID, auditor.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RemoveRole(absent) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationResolverPermissions(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	member := newMember(t, ctx, pool, org.ID)
	resolver := NewAccessResolver(pool)

	// No roles yet: no permissions, fail-closed.
	if ok, err := resolver.HasPermission(ctx, member.ID, PermAuditView); err != nil || ok {
		t.Fatalf("HasPermission(no roles) = %v, %v; want false, nil", ok, err)
	}

	// Direct Auditor role grants audit.view.
	if _, err := NewMemberRoleStore(pool).AssignRole(ctx, member.ID, builtins[BuiltinAuditor].ID); err != nil {
		t.Fatalf("assign auditor: %v", err)
	}
	if ok, err := resolver.HasPermission(ctx, member.ID, PermAuditView); err != nil || !ok {
		t.Fatalf("HasPermission(audit.view) = %v, %v; want true", ok, err)
	}

	// A team-assigned Admin role is inherited via team membership (union).
	team, err := NewTeamStore(pool).Create(ctx, org.ID, "admins")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := NewTeamMembershipStore(pool).Add(ctx, team.ID, member.ID); err != nil {
		t.Fatalf("add membership: %v", err)
	}
	if _, err := NewTeamRoleStore(pool).AssignRole(ctx, team.ID, builtins[BuiltinAdmin].ID); err != nil {
		t.Fatalf("assign admin to team: %v", err)
	}
	if ok, err := resolver.HasPermission(ctx, member.ID, PermMembersManage); err != nil || !ok {
		t.Fatalf("HasPermission(members.manage via team) = %v, %v; want true", ok, err)
	}

	// Effective permissions are the union of Auditor + Admin, deduped, sorted.
	eff, err := resolver.EffectivePermissions(ctx, member.ID)
	if err != nil {
		t.Fatalf("EffectivePermissions: %v", err)
	}
	// Admin has 8 perms incl. audit.view; Auditor adds nothing new -> 8 distinct.
	if len(eff) != 8 {
		t.Fatalf("EffectivePermissions len = %d (%v), want 8", len(eff), eff)
	}
	if hasPerm(eff, PermAgentsRun) {
		t.Fatalf("effective perms unexpectedly include agents.run: %v", eff)
	}
	for i := 1; i < len(eff); i++ {
		if eff[i-1] >= eff[i] {
			t.Fatalf("EffectivePermissions not sorted/deduped: %v", eff)
		}
	}
}
