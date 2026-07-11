//go:build integration

// Integration coverage for the catalog / model-infra / billing repositories,
// mirroring integration_test.go: happy-path create/get/list plus the not-found
// and conflict paths for every aggregate. They share the helpers in
// integration_test.go (requirePool, newOrg, uniq) and skip when Postgres is
// unreachable. Each test hangs its rows off a throwaway organization whose
// cleanup cascades, so nothing leaks between tests.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

const missingUUID = "00000000-0000-0000-0000-000000000000"

// mustNumeric builds a valid pgtype.Numeric from a decimal string.
func mustNumeric(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		t.Fatalf("build numeric %q: %v", s, err)
	}
	return n
}

// wantNumeric asserts got is a valid numeric equal (as a rational) to want.
func wantNumeric(t *testing.T, got pgtype.Numeric, want string) {
	t.Helper()
	if !got.Valid {
		t.Fatalf("numeric = NULL, want %s", want)
	}
	v, err := got.Value()
	if err != nil {
		t.Fatalf("numeric.Value: %v", err)
	}
	gr, ok := new(big.Rat).SetString(v.(string))
	if !ok {
		t.Fatalf("cannot parse round-tripped numeric %q", v)
	}
	wr, _ := new(big.Rat).SetString(want)
	if gr.Cmp(wr) != 0 {
		t.Fatalf("numeric = %s, want %s", v, want)
	}
}

// newMember creates a throwaway member in orgID. db is typically a *db.Pool,
// which satisfies Querier.
func newMember(t *testing.T, ctx context.Context, db Querier, orgID string) Member {
	t.Helper()
	m, err := NewMemberStore(db).Create(ctx, CreateMemberParams{
		OrgID: orgID, IDPSubject: uniq("idp"), Email: uniq("m") + "@example.com",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	return m
}

func TestIntegrationModelEndpoints(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	endpoints := NewModelEndpointStore(pool)

	created, err := endpoints.Create(ctx, CreateModelEndpointParams{
		OrgID: org.ID, Name: "anthropic", BaseURL: "https://api.anthropic.com", Provider: "anthropic", SecretRef: "vault://k1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || !created.Active || created.SecretRef != "vault://k1" || created.CreatedAt.IsZero() {
		t.Fatalf("Create returned incomplete row: %+v", created)
	}
	if _, err := endpoints.Create(ctx, CreateModelEndpointParams{
		OrgID: org.ID, Name: "openai", BaseURL: "https://api.openai.com", Provider: "openai", SecretRef: "vault://k2",
	}); err != nil {
		t.Fatalf("Create openai: %v", err)
	}

	got, err := endpoints.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != created {
		t.Fatalf("GetByID = %+v, want %+v", got, created)
	}

	list, err := endpoints.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].Name != "anthropic" || list[1].Name != "openai" {
		t.Fatalf("ListByOrg = %+v, want [anthropic openai] ordered by name", list)
	}

	deactivated, err := endpoints.SetActive(ctx, created.ID, false)
	if err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if deactivated.Active {
		t.Fatal("SetActive(false) left endpoint active")
	}

	// Duplicate (org_id, name) -> ErrConflict.
	if _, err := endpoints.Create(ctx, CreateModelEndpointParams{
		OrgID: org.ID, Name: "anthropic", BaseURL: "x", Provider: "anthropic", SecretRef: "vault://dupe",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := endpoints.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if _, err := endpoints.SetActive(ctx, missingUUID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetActive(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationRoutingRules(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	endpoint, err := NewModelEndpointStore(pool).Create(ctx, CreateModelEndpointParams{
		OrgID: org.ID, Name: "ep", BaseURL: "https://x", Provider: "anthropic", SecretRef: "vault://k",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	rules := NewRoutingRuleStore(pool)

	// A nil MatchJSON falls back to the column default '{}'.
	defaulted, err := rules.Create(ctx, CreateRoutingRuleParams{OrgID: org.ID, Priority: 10, EndpointID: endpoint.ID})
	if err != nil {
		t.Fatalf("Create defaulted: %v", err)
	}
	if string(defaulted.MatchJSON) != "{}" {
		t.Fatalf("default MatchJSON = %q, want {}", defaulted.MatchJSON)
	}
	first, err := rules.Create(ctx, CreateRoutingRuleParams{
		OrgID: org.ID, Priority: 1, MatchJSON: json.RawMessage(`{"model":"claude"}`), EndpointID: endpoint.ID,
	})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}

	list, err := rules.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].ID != first.ID || list[1].ID != defaulted.ID {
		t.Fatalf("ListByOrg not ordered by priority ascending: %+v", list)
	}

	if err := rules.Delete(ctx, first.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := rules.Delete(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(absent) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationBudgets(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	budgets := NewBudgetStore(pool)

	// Empty currency defaults to USD.
	created, err := budgets.Create(ctx, org.ID, "monthly", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Currency != "USD" {
		t.Fatalf("default currency = %q, want USD", created.Currency)
	}
	if _, err := budgets.Create(ctx, org.ID, "annual", "EUR"); err != nil {
		t.Fatalf("Create annual: %v", err)
	}

	got, err := budgets.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != created {
		t.Fatalf("GetByID = %+v, want %+v", got, created)
	}

	list, err := budgets.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].Name != "annual" || list[1].Name != "monthly" {
		t.Fatalf("ListByOrg = %+v, want [annual monthly] ordered by name", list)
	}

	if _, err := budgets.Create(ctx, org.ID, "monthly", "USD"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := budgets.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationBudgetRules(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	budget, err := NewBudgetStore(pool).Create(ctx, org.ID, "b", "USD")
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	rules := NewBudgetRuleStore(pool)

	scopeID := org.ID
	withCaps, err := rules.Create(ctx, CreateBudgetRuleParams{
		BudgetID: budget.ID, Scope: BudgetScopeOrganization, ScopeID: &scopeID,
		Window: BudgetWindowMonthly, SoftCap: mustNumeric(t, "100.5"), HardCap: mustNumeric(t, "250"),
	})
	if err != nil {
		t.Fatalf("Create withCaps: %v", err)
	}
	wantNumeric(t, withCaps.SoftCap, "100.5")
	wantNumeric(t, withCaps.HardCap, "250")
	if withCaps.ScopeID == nil || *withCaps.ScopeID != scopeID {
		t.Fatalf("ScopeID = %v, want %s", withCaps.ScopeID, scopeID)
	}

	// NULL caps and NULL scope_id round-trip as invalid/nil.
	nullCaps, err := rules.Create(ctx, CreateBudgetRuleParams{
		BudgetID: budget.ID, Scope: BudgetScopeAgent, Window: BudgetWindowDaily,
	})
	if err != nil {
		t.Fatalf("Create nullCaps: %v", err)
	}
	if nullCaps.SoftCap.Valid || nullCaps.HardCap.Valid || nullCaps.ScopeID != nil {
		t.Fatalf("nullCaps expected NULL caps and scope_id: %+v", nullCaps)
	}

	list, err := rules.ListByBudget(ctx, budget.ID)
	if err != nil {
		t.Fatalf("ListByBudget: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListByBudget len = %d, want 2", len(list))
	}

	if err := rules.Delete(ctx, withCaps.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := rules.Delete(ctx, withCaps.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(absent) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationPolicies(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	policies := NewPolicyStore(pool)

	desc := "governs tool use"
	created, err := policies.Create(ctx, org.ID, "default", &desc)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.CurrentVersion != 0 || created.Description == nil || *created.Description != desc {
		t.Fatalf("Create returned unexpected row: %+v", created)
	}
	// Nil description stores NULL.
	if _, err := policies.Create(ctx, org.ID, "strict", nil); err != nil {
		t.Fatalf("Create strict: %v", err)
	}

	got, err := policies.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("GetByID = %+v, want id %s", got, created.ID)
	}

	list, err := policies.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].Name != "default" || list[1].Name != "strict" {
		t.Fatalf("ListByOrg = %+v, want [default strict] ordered by name", list)
	}
	if list[1].Description != nil {
		t.Fatalf("strict Description = %v, want nil", *list[1].Description)
	}

	updated, err := policies.SetCurrentVersion(ctx, created.ID, 3)
	if err != nil {
		t.Fatalf("SetCurrentVersion: %v", err)
	}
	if updated.CurrentVersion != 3 {
		t.Fatalf("CurrentVersion = %d, want 3", updated.CurrentVersion)
	}

	if _, err := policies.Create(ctx, org.ID, "default", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := policies.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if _, err := policies.SetCurrentVersion(ctx, missingUUID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCurrentVersion(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationPolicyVersions(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	policy, err := NewPolicyStore(pool).Create(ctx, org.ID, "p", nil)
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	versions := NewPolicyVersionStore(pool)

	v1, err := versions.Create(ctx, CreatePolicyVersionParams{
		PolicyID: policy.ID, Version: 1, ContentJSON: json.RawMessage(`{"tools":[]}`), ContentHash: "hash1",
	})
	if err != nil {
		t.Fatalf("Create v1: %v", err)
	}
	if v1.SigningJSON != nil {
		t.Fatalf("v1 SigningJSON = %q, want nil", v1.SigningJSON)
	}
	if _, err := versions.Create(ctx, CreatePolicyVersionParams{
		PolicyID: policy.ID, Version: 2, ContentJSON: json.RawMessage(`{"tools":["fs.read"]}`),
		ContentHash: "hash2", SigningJSON: json.RawMessage(`{"alg":"ed25519"}`),
	}); err != nil {
		t.Fatalf("Create v2: %v", err)
	}

	got, err := versions.GetByPolicyAndVersion(ctx, policy.ID, 1)
	if err != nil {
		t.Fatalf("GetByPolicyAndVersion: %v", err)
	}
	if got.ID != v1.ID || got.ContentHash != "hash1" {
		t.Fatalf("GetByPolicyAndVersion = %+v, want v1 %s", got, v1.ID)
	}

	list, err := versions.ListByPolicy(ctx, policy.ID)
	if err != nil {
		t.Fatalf("ListByPolicy: %v", err)
	}
	if len(list) != 2 || list[0].Version != 1 || list[1].Version != 2 {
		t.Fatalf("ListByPolicy = %+v, want versions [1 2]", list)
	}

	// Duplicate (policy_id, version) -> ErrConflict.
	if _, err := versions.Create(ctx, CreatePolicyVersionParams{
		PolicyID: policy.ID, Version: 1, ContentJSON: json.RawMessage(`{}`), ContentHash: "dupe",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := versions.GetByPolicyAndVersion(ctx, policy.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByPolicyAndVersion(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationAgents(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agents := NewAgentStore(pool)

	created, err := agents.Create(ctx, org.ID, "summarizer", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.CurrentVersion != 0 || created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("Create returned unexpected row: %+v", created)
	}
	if _, err := agents.Create(ctx, org.ID, "researcher", nil); err != nil {
		t.Fatalf("Create researcher: %v", err)
	}

	got, err := agents.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got != created {
		t.Fatalf("GetByID = %+v, want %+v", got, created)
	}

	list, err := agents.ListByOrg(ctx, org.ID)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(list) != 2 || list[0].Name != "researcher" || list[1].Name != "summarizer" {
		t.Fatalf("ListByOrg = %+v, want [researcher summarizer] ordered by name", list)
	}

	updated, err := agents.SetCurrentVersion(ctx, created.ID, 2)
	if err != nil {
		t.Fatalf("SetCurrentVersion: %v", err)
	}
	if updated.CurrentVersion != 2 {
		t.Fatalf("CurrentVersion = %d, want 2", updated.CurrentVersion)
	}
	if updated.UpdatedAt.Before(created.UpdatedAt) {
		t.Fatalf("SetCurrentVersion moved updated_at backwards: %v < %v", updated.UpdatedAt, created.UpdatedAt)
	}

	if _, err := agents.Create(ctx, org.ID, "summarizer", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := agents.GetByID(ctx, missingUUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID(missing) = %v, want ErrNotFound", err)
	}
	if _, err := agents.SetCurrentVersion(ctx, missingUUID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCurrentVersion(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationAgentVersions(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent, err := NewAgentStore(pool).Create(ctx, org.ID, "a", nil)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	versions := NewAgentVersionStore(pool)

	// A nil AllowedTools falls back to '[]'; nil pins store NULL.
	v1, err := versions.Create(ctx, CreateAgentVersionParams{AgentID: agent.ID, Version: 1, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("Create v1: %v", err)
	}
	if string(v1.AllowedTools) != "[]" || v1.SystemPrompt != "" || v1.FanOutCeiling != 1 {
		t.Fatalf("v1 defaults unexpected: %+v", v1)
	}
	if v1.PolicyVersionID != nil || v1.EndpointID != nil || v1.BudgetID != nil {
		t.Fatalf("v1 pins should be nil: %+v", v1)
	}

	// A version that pins real building blocks.
	endpoint, err := NewModelEndpointStore(pool).Create(ctx, CreateModelEndpointParams{
		OrgID: org.ID, Name: "ep", BaseURL: "https://x", Provider: "anthropic", SecretRef: "vault://k",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	budget, err := NewBudgetStore(pool).Create(ctx, org.ID, "b", "USD")
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	v2, err := versions.Create(ctx, CreateAgentVersionParams{
		AgentID: agent.ID, Version: 2, SystemPrompt: "be helpful",
		AllowedTools: json.RawMessage(`["fs.read"]`), EndpointID: &endpoint.ID, BudgetID: &budget.ID, FanOutCeiling: 4,
	})
	if err != nil {
		t.Fatalf("Create v2: %v", err)
	}
	if v2.EndpointID == nil || *v2.EndpointID != endpoint.ID || v2.BudgetID == nil || *v2.BudgetID != budget.ID {
		t.Fatalf("v2 pins not stored: %+v", v2)
	}

	got, err := versions.GetByAgentAndVersion(ctx, agent.ID, 2)
	if err != nil {
		t.Fatalf("GetByAgentAndVersion: %v", err)
	}
	if got.ID != v2.ID || got.SystemPrompt != "be helpful" {
		t.Fatalf("GetByAgentAndVersion = %+v, want v2 %s", got, v2.ID)
	}

	list, err := versions.ListByAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("ListByAgent: %v", err)
	}
	if len(list) != 2 || list[0].Version != 1 || list[1].Version != 2 {
		t.Fatalf("ListByAgent = %+v, want versions [1 2]", list)
	}

	if _, err := versions.Create(ctx, CreateAgentVersionParams{AgentID: agent.ID, Version: 1, FanOutCeiling: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create error = %v, want ErrConflict", err)
	}
	if _, err := versions.GetByAgentAndVersion(ctx, agent.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByAgentAndVersion(missing) = %v, want ErrNotFound", err)
	}
}

func TestIntegrationEntitlements(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	agent, err := NewAgentStore(pool).Create(ctx, org.ID, "a", nil)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	member := newMember(t, ctx, pool, org.ID)
	team, err := NewTeamStore(pool).Create(ctx, org.ID, "platform")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	ents := NewEntitlementStore(pool)

	fanOut := 8
	byMember, err := ents.Create(ctx, CreateEntitlementParams{AgentID: agent.ID, MemberID: &member.ID, MaxFanOut: &fanOut})
	if err != nil {
		t.Fatalf("Create by member: %v", err)
	}
	if byMember.MemberID == nil || *byMember.MemberID != member.ID || byMember.TeamID != nil {
		t.Fatalf("member grant stored wrong principal: %+v", byMember)
	}
	if byMember.MaxFanOut == nil || *byMember.MaxFanOut != fanOut {
		t.Fatalf("MaxFanOut = %v, want %d", byMember.MaxFanOut, fanOut)
	}
	byTeam, err := ents.Create(ctx, CreateEntitlementParams{AgentID: agent.ID, TeamID: &team.ID})
	if err != nil {
		t.Fatalf("Create by team: %v", err)
	}
	if byTeam.TeamID == nil || *byTeam.TeamID != team.ID || byTeam.MemberID != nil {
		t.Fatalf("team grant stored wrong principal: %+v", byTeam)
	}

	// Bad principals are rejected before the DB, so no row is written.
	if _, err := ents.Create(ctx, CreateEntitlementParams{AgentID: agent.ID}); err == nil {
		t.Fatal("Create with neither member nor team = nil error, want validation error")
	}
	if _, err := ents.Create(ctx, CreateEntitlementParams{AgentID: agent.ID, MemberID: &member.ID, TeamID: &team.ID}); err == nil {
		t.Fatal("Create with both member and team = nil error, want validation error")
	}

	byAgent, err := ents.ListByAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("ListByAgent: %v", err)
	}
	if len(byAgent) != 2 {
		t.Fatalf("ListByAgent len = %d, want 2", len(byAgent))
	}
	memberList, err := ents.ListByMember(ctx, member.ID)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(memberList) != 1 || memberList[0].ID != byMember.ID {
		t.Fatalf("ListByMember = %+v, want [%s]", memberList, byMember.ID)
	}
	teamList, err := ents.ListByTeam(ctx, team.ID)
	if err != nil {
		t.Fatalf("ListByTeam: %v", err)
	}
	if len(teamList) != 1 || teamList[0].ID != byTeam.ID {
		t.Fatalf("ListByTeam = %+v, want [%s]", teamList, byTeam.ID)
	}

	// Duplicate (agent, member) grant -> ErrConflict.
	if _, err := ents.Create(ctx, CreateEntitlementParams{AgentID: agent.ID, MemberID: &member.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate member grant error = %v, want ErrConflict", err)
	}

	if err := ents.Delete(ctx, byMember.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := ents.Delete(ctx, byMember.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(absent) = %v, want ErrNotFound", err)
	}
}
