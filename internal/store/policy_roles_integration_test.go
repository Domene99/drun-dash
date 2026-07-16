//go:build integration

// Integration coverage for the policy_roles store and the policy-centric access
// model (ADR-0010): AuthorizeRun and ListAccessibleAgentIDs resolved against a
// live schema. Shares the requirePool / newOrg / newMember / seedBuiltins
// helpers with the other *_integration_test.go files and skips when Postgres is
// unreachable.
package store

import (
	"context"
	"testing"
)

// newPolicyWithVersion creates a policy in orgID with a single (unsigned)
// version and advances its current-version pointer to it. It returns the policy
// and the id of that version, which an agent version pins to make the policy the
// agent's governing policy.
func newPolicyWithVersion(t *testing.T, ctx context.Context, db Querier, orgID, name string) (Policy, string) {
	t.Helper()
	ps := NewPolicyStore(db)
	p, err := ps.Create(ctx, orgID, name, nil)
	if err != nil {
		t.Fatalf("create policy %q: %v", name, err)
	}
	pv, err := NewPolicyVersionStore(db).Create(ctx, CreatePolicyVersionParams{
		PolicyID: p.ID, Version: 1, ContentJSON: []byte("{}"), ContentHash: "sha256:test",
	})
	if err != nil {
		t.Fatalf("create policy version for %q: %v", name, err)
	}
	if _, err := ps.SetCurrentVersion(ctx, p.ID, 1); err != nil {
		t.Fatalf("set current version for %q: %v", name, err)
	}
	return p, pv.ID
}

// newAgentGoverned creates an agent in orgID whose current version pins
// policyVersionID as its governing policy. Pass "" for policyVersionID to leave
// the version's policy pin NULL — an agent with no governing policy (general
// access).
func newAgentGoverned(t *testing.T, ctx context.Context, db Querier, orgID, name, policyVersionID string) Agent {
	t.Helper()
	as := NewAgentStore(db)
	a, err := as.Create(ctx, orgID, name, nil)
	if err != nil {
		t.Fatalf("create agent %q: %v", name, err)
	}
	var pvid *string
	if policyVersionID != "" {
		pvid = &policyVersionID
	}
	if _, err := NewAgentVersionStore(db).Create(ctx, CreateAgentVersionParams{
		AgentID: a.ID, Version: 1, PolicyVersionID: pvid, FanOutCeiling: 1,
	}); err != nil {
		t.Fatalf("create agent version for %q: %v", name, err)
	}
	updated, err := as.SetCurrentVersion(ctx, a.ID, 1)
	if err != nil {
		t.Fatalf("set current version for agent %q: %v", name, err)
	}
	return updated
}

func TestIntegrationPolicyRoleStore(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	roles := NewRoleStore(pool)
	prs := NewPolicyRoleStore(pool)

	roleA, err := roles.Create(ctx, CreateRoleParams{OrgID: org.ID, Name: "A", Permissions: []Permission{PermAgentsRun}})
	if err != nil {
		t.Fatalf("create role A: %v", err)
	}
	roleB, err := roles.Create(ctx, CreateRoleParams{OrgID: org.ID, Name: "B", Permissions: []Permission{PermAgentsRun}})
	if err != nil {
		t.Fatalf("create role B: %v", err)
	}
	p1, _ := newPolicyWithVersion(t, ctx, pool, org.ID, "p1")
	p2, _ := newPolicyWithVersion(t, ctx, pool, org.ID, "p2")

	// Empty on a fresh policy.
	if ids, err := prs.ListRoles(ctx, p1.ID); err != nil || len(ids) != 0 {
		t.Fatalf("ListRoles(fresh) = %v, %v; want [], nil", ids, err)
	}

	// SetRoles replaces wholesale and dedupes.
	if err := prs.SetRoles(ctx, p1.ID, []string{roleA.ID, roleB.ID, roleA.ID}); err != nil {
		t.Fatalf("SetRoles(p1): %v", err)
	}
	got, err := prs.ListRoles(ctx, p1.ID)
	if err != nil {
		t.Fatalf("ListRoles(p1): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListRoles(p1) = %v, want 2 distinct (deduped)", got)
	}

	// Replace-all: setting a smaller set drops the rest.
	if err := prs.SetRoles(ctx, p1.ID, []string{roleA.ID}); err != nil {
		t.Fatalf("SetRoles(p1, [A]): %v", err)
	}
	if got, _ := prs.ListRoles(ctx, p1.ID); len(got) != 1 || got[0] != roleA.ID {
		t.Fatalf("ListRoles after replace = %v, want [A]", got)
	}

	// p2 permits roleA too.
	if err := prs.SetRoles(ctx, p2.ID, []string{roleA.ID}); err != nil {
		t.Fatalf("SetRoles(p2): %v", err)
	}

	// ListPoliciesForRoles: roleA reaches p1 and p2; roleB reaches none now.
	forA, err := prs.ListPoliciesForRoles(ctx, []string{roleA.ID})
	if err != nil {
		t.Fatalf("ListPoliciesForRoles(A): %v", err)
	}
	if len(forA) != 2 {
		t.Fatalf("ListPoliciesForRoles(A) = %v, want {p1, p2}", forA)
	}
	forB, err := prs.ListPoliciesForRoles(ctx, []string{roleB.ID})
	if err != nil {
		t.Fatalf("ListPoliciesForRoles(B): %v", err)
	}
	if len(forB) != 0 {
		t.Fatalf("ListPoliciesForRoles(B) = %v, want empty", forB)
	}
	// Empty input short-circuits to empty.
	if ids, err := prs.ListPoliciesForRoles(ctx, nil); err != nil || len(ids) != 0 {
		t.Fatalf("ListPoliciesForRoles(nil) = %v, %v; want [], nil", ids, err)
	}

	// An unknown role id is rejected by the foreign key.
	if err := prs.SetRoles(ctx, p1.ID, []string{missingUUID}); err == nil {
		t.Fatal("SetRoles(unknown role) = nil, want a foreign-key error")
	}
	// The failed transaction rolled back — p1 still permits roleA.
	if got, _ := prs.ListRoles(ctx, p1.ID); len(got) != 1 || got[0] != roleA.ID {
		t.Fatalf("ListRoles after failed SetRoles = %v, want [A] (rolled back)", got)
	}

	// Clearing to empty is valid.
	if err := prs.SetRoles(ctx, p1.ID, nil); err != nil {
		t.Fatalf("SetRoles(p1, nil): %v", err)
	}
	if got, _ := prs.ListRoles(ctx, p1.ID); len(got) != 0 {
		t.Fatalf("ListRoles after clear = %v, want []", got)
	}
}

func TestIntegrationResolverAuthorizeRunPolicyCentric(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	member := newMember(t, ctx, pool, org.ID)
	roles := NewRoleStore(pool)
	mrs := NewMemberRoleStore(pool)
	prs := NewPolicyRoleStore(pool)
	resolver := NewAccessResolver(pool)

	freeAgent := newAgentGoverned(t, ctx, pool, org.ID, "free", "")
	policyP, pvid := newPolicyWithVersion(t, ctx, pool, org.ID, "guard")
	gatedAgent := newAgentGoverned(t, ctx, pool, org.ID, "gated", pvid)

	// 1. No agents.run: everything is denied.
	for _, ag := range []Agent{freeAgent, gatedAgent} {
		if _, ok, err := resolver.AuthorizeRun(ctx, member.ID, ag.ID); err != nil || ok {
			t.Fatalf("AuthorizeRun(no perms, %s) = ok %v, err %v; want false", ag.ID, ok, err)
		}
	}

	// 2. Built-in Member grants agents.run. The policy-less agent is general
	// access → allowed; the governed agent whose policy permits no role → denied.
	if _, err := mrs.AssignRole(ctx, member.ID, builtins[BuiltinMember].ID); err != nil {
		t.Fatalf("assign member role: %v", err)
	}
	if _, ok, err := resolver.AuthorizeRun(ctx, member.ID, freeAgent.ID); err != nil || !ok {
		t.Fatalf("AuthorizeRun(free, general access) = ok %v, err %v; want true", ok, err)
	}
	if _, ok, err := resolver.AuthorizeRun(ctx, member.ID, gatedAgent.ID); err != nil || ok {
		t.Fatalf("AuthorizeRun(gated, policy permits no role) = ok %v, err %v; want false", ok, err)
	}

	// 3. Permitting the Member role on the governing policy authorizes the gated
	// agent — via policy, not all_agents.
	if err := prs.SetRoles(ctx, policyP.ID, []string{builtins[BuiltinMember].ID}); err != nil {
		t.Fatalf("SetRoles(guard): %v", err)
	}
	grant, ok, err := resolver.AuthorizeRun(ctx, member.ID, gatedAgent.ID)
	if err != nil || !ok {
		t.Fatalf("AuthorizeRun(gated, policy permits Member) = ok %v, err %v; want true", ok, err)
	}
	if grant.AllAgents {
		t.Fatal("grant.AllAgents = true, want false (reached via policy)")
	}

	// 4. A separate locked policy permits nobody → its agent is denied, until an
	// all_agents super-admin role authorizes any agent regardless of policy.
	_, lpvid := newPolicyWithVersion(t, ctx, pool, org.ID, "locked")
	lockedAgent := newAgentGoverned(t, ctx, pool, org.ID, "locked-agent", lpvid)
	if _, ok, err := resolver.AuthorizeRun(ctx, member.ID, lockedAgent.ID); err != nil || ok {
		t.Fatalf("AuthorizeRun(locked, before all_agents) = ok %v, err %v; want false", ok, err)
	}
	allRole, err := roles.Create(ctx, CreateRoleParams{
		OrgID: org.ID, Name: "runs-all", AllAgents: true, Permissions: []Permission{PermAgentsRun},
	})
	if err != nil {
		t.Fatalf("create all-agents role: %v", err)
	}
	if _, err := mrs.AssignRole(ctx, member.ID, allRole.ID); err != nil {
		t.Fatalf("assign all-agents role: %v", err)
	}
	grantAll, ok, err := resolver.AuthorizeRun(ctx, member.ID, lockedAgent.ID)
	if err != nil || !ok {
		t.Fatalf("AuthorizeRun(locked via all_agents) = ok %v, err %v; want true", ok, err)
	}
	if !grantAll.AllAgents {
		t.Fatalf("all_agents grant expected: %+v", grantAll)
	}
}

func TestIntegrationResolverListAccessibleAgentIDs(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	builtins := seedBuiltins(t, ctx, pool, org.ID)
	member := newMember(t, ctx, pool, org.ID)
	roles := NewRoleStore(pool)
	mrs := NewMemberRoleStore(pool)
	prs := NewPolicyRoleStore(pool)
	resolver := NewAccessResolver(pool)

	freeAgent := newAgentGoverned(t, ctx, pool, org.ID, "free", "")
	policyP, pvid := newPolicyWithVersion(t, ctx, pool, org.ID, "guard")
	gatedAgent := newAgentGoverned(t, ctx, pool, org.ID, "gated", pvid)
	_, lpvid := newPolicyWithVersion(t, ctx, pool, org.ID, "locked")
	lockedAgent := newAgentGoverned(t, ctx, pool, org.ID, "locked-agent", lpvid)

	asSet := func(ids []string) map[string]bool {
		m := make(map[string]bool, len(ids))
		for _, id := range ids {
			m[id] = true
		}
		return m
	}

	// No agents.run → empty set.
	if ids, err := resolver.ListAccessibleAgentIDs(ctx, member.ID, org.ID); err != nil || len(ids) != 0 {
		t.Fatalf("ListAccessibleAgentIDs(no run) = %v, %v; want [], nil", ids, err)
	}

	// Built-in Member: the policy-less agent is general access; the governed
	// agents (permitting no role) are not visible.
	if _, err := mrs.AssignRole(ctx, member.ID, builtins[BuiltinMember].ID); err != nil {
		t.Fatalf("assign member role: %v", err)
	}
	ids, err := resolver.ListAccessibleAgentIDs(ctx, member.ID, org.ID)
	if err != nil {
		t.Fatalf("ListAccessibleAgentIDs(member): %v", err)
	}
	set := asSet(ids)
	if !set[freeAgent.ID] || set[gatedAgent.ID] || set[lockedAgent.ID] {
		t.Fatalf("visible = %v, want only the policy-less agent", ids)
	}

	// A team role reaching the guard policy makes the gated agent visible: create
	// a team the member belongs to, assign it a role, and permit that role on the
	// policy (team roles are part of the reachable set).
	team, err := NewTeamStore(pool).Create(ctx, org.ID, "platform")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := NewTeamMembershipStore(pool).Add(ctx, team.ID, member.ID); err != nil {
		t.Fatalf("add team membership: %v", err)
	}
	teamRole, err := roles.Create(ctx, CreateRoleParams{OrgID: org.ID, Name: "team-runs", Permissions: []Permission{PermAgentsRun}})
	if err != nil {
		t.Fatalf("create team role: %v", err)
	}
	if _, err := NewTeamRoleStore(pool).AssignRole(ctx, team.ID, teamRole.ID); err != nil {
		t.Fatalf("assign team role: %v", err)
	}
	if err := prs.SetRoles(ctx, policyP.ID, []string{teamRole.ID}); err != nil {
		t.Fatalf("SetRoles(guard, team role): %v", err)
	}
	ids, err = resolver.ListAccessibleAgentIDs(ctx, member.ID, org.ID)
	if err != nil {
		t.Fatalf("ListAccessibleAgentIDs(team role): %v", err)
	}
	set = asSet(ids)
	if !set[freeAgent.ID] || !set[gatedAgent.ID] || set[lockedAgent.ID] {
		t.Fatalf("visible = %v, want free + gated (via team role), not locked", ids)
	}

	// An all_agents role makes every org agent visible, including the locked one.
	allRole, err := roles.Create(ctx, CreateRoleParams{
		OrgID: org.ID, Name: "runs-all", AllAgents: true, Permissions: []Permission{PermAgentsRun},
	})
	if err != nil {
		t.Fatalf("create all-agents role: %v", err)
	}
	if _, err := mrs.AssignRole(ctx, member.ID, allRole.ID); err != nil {
		t.Fatalf("assign all-agents role: %v", err)
	}
	ids, err = resolver.ListAccessibleAgentIDs(ctx, member.ID, org.ID)
	if err != nil {
		t.Fatalf("ListAccessibleAgentIDs(all_agents): %v", err)
	}
	set = asSet(ids)
	if !set[freeAgent.ID] || !set[gatedAgent.ID] || !set[lockedAgent.ID] {
		t.Fatalf("visible = %v, want all three agents via all_agents", ids)
	}
}
