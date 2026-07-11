//go:build integration

// Integration coverage for the analytics aggregation queries (AnalyticsStore):
// spend rollups over usage_events (by agent / policy / principal) and
// current-window budget utilisation. Mirrors the other *_integration_test.go
// files and reuses their helpers (requirePool, newOrg, newAgent, newMember,
// uniq, mustNumeric, wantNumeric). Each test hangs its rows off a throwaway
// organization whose cleanup cascades, so nothing leaks between tests.
package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// insertUsage appends a usage_events row with an explicit occurred_at so tests
// can place events precisely inside or outside a report window. Append (the
// production path) always stamps now(), so the analytics tests bypass it and
// insert directly.
func insertUsage(t *testing.T, ctx context.Context, db Querier, orgID string, sessionID, agentID *string, cost string, prompt, completion int, at time.Time) {
	t.Helper()
	_, err := db.Exec(ctx, `
		INSERT INTO usage_events (org_id, session_id, agent_id, prompt_tokens, completion_tokens, cost_amount, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7)`,
		orgID, sessionID, agentID, prompt, completion, cost, at)
	if err != nil {
		t.Fatalf("insert usage event: %v", err)
	}
}

func TestIntegrationSpendByOrg(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	a1 := newAgent(t, ctx, pool, org.ID)
	a2 := newAgent(t, ctx, pool, org.ID)
	member := newMember(t, ctx, pool, org.ID)

	policy, err := NewPolicyStore(pool).Create(ctx, org.ID, uniq("policy"), nil)
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	pv, err := NewPolicyVersionStore(pool).Create(ctx, CreatePolicyVersionParams{
		PolicyID: policy.ID, Version: 1, ContentJSON: json.RawMessage(`{}`), ContentHash: uniq("h"),
	})
	if err != nil {
		t.Fatalf("create policy version: %v", err)
	}
	// s1 carries both a policy version and a launching principal, so a1's usage
	// is attributable by policy and by principal. a2's usage has no session.
	s1, err := NewSessionStore(pool).Create(ctx, CreateSessionParams{
		OrgID: org.ID, AgentID: a1.ID, PolicyVersionID: &pv.ID, LaunchedBy: &member.ID, FanOutCeiling: 1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(2 * time.Hour)
	inside := now.Add(-1 * time.Hour)
	before := now.Add(-3 * time.Hour) // earlier than `from`, must be excluded

	// a1: two in-window events via s1 -> 4.000000 spend, 150 tokens.
	insertUsage(t, ctx, pool, org.ID, &s1.ID, &a1.ID, "1.5", 60, 40, inside)
	insertUsage(t, ctx, pool, org.ID, &s1.ID, &a1.ID, "2.5", 30, 20, inside)
	// a2: one in-window event, no session -> 3.000000 spend, 20 tokens.
	insertUsage(t, ctx, pool, org.ID, nil, &a2.ID, "3", 10, 10, inside)
	// Out-of-window event (before `from`): must never be counted.
	insertUsage(t, ctx, pool, org.ID, &s1.ID, &a1.ID, "99", 1, 1, before)

	analytics := NewAnalyticsStore(pool)

	// group_by = agent (also the default for ""): every event buckets by its
	// agent_id, so Total == sum of groups == 7.000000.
	for _, gb := range []string{SpendGroupByAgent, ""} {
		got, err := analytics.SpendByOrg(ctx, org.ID, from, to, gb)
		if err != nil {
			t.Fatalf("SpendByOrg(%q): %v", gb, err)
		}
		wantNumeric(t, got.Total, "7")
		if got.Currency != "USD" {
			t.Fatalf("Currency = %q, want USD", got.Currency)
		}
		if len(got.Groups) != 2 {
			t.Fatalf("groups = %+v, want 2 (a1, a2)", got.Groups)
		}
		// Ordered by spend descending: a1 (4) before a2 (3).
		if got.Groups[0].Key != a1.ID {
			t.Fatalf("groups[0].Key = %q, want a1 %q", got.Groups[0].Key, a1.ID)
		}
		wantNumeric(t, got.Groups[0].Amount, "4")
		if got.Groups[0].Tokens != 150 {
			t.Fatalf("a1 tokens = %d, want 150", got.Groups[0].Tokens)
		}
		if got.Groups[1].Key != a2.ID {
			t.Fatalf("groups[1].Key = %q, want a2 %q", got.Groups[1].Key, a2.ID)
		}
		wantNumeric(t, got.Groups[1].Amount, "3")
		if got.Groups[1].Tokens != 20 {
			t.Fatalf("a2 tokens = %d, want 20", got.Groups[1].Tokens)
		}
	}

	// group_by = policy: only a1's usage joins a session with a policy version,
	// so there is a single group keyed by the policy id. Total stays org-wide.
	byPolicy, err := analytics.SpendByOrg(ctx, org.ID, from, to, SpendGroupByPolicy)
	if err != nil {
		t.Fatalf("SpendByOrg(policy): %v", err)
	}
	wantNumeric(t, byPolicy.Total, "7")
	if len(byPolicy.Groups) != 1 || byPolicy.Groups[0].Key != policy.ID {
		t.Fatalf("policy groups = %+v, want single group keyed by policy %q", byPolicy.Groups, policy.ID)
	}
	wantNumeric(t, byPolicy.Groups[0].Amount, "4")
	if byPolicy.Groups[0].Tokens != 150 {
		t.Fatalf("policy tokens = %d, want 150", byPolicy.Groups[0].Tokens)
	}

	// group_by = principal: a1's usage joins s1, launched by `member`.
	byPrincipal, err := analytics.SpendByOrg(ctx, org.ID, from, to, SpendGroupByPrincipal)
	if err != nil {
		t.Fatalf("SpendByOrg(principal): %v", err)
	}
	if len(byPrincipal.Groups) != 1 || byPrincipal.Groups[0].Key != member.ID {
		t.Fatalf("principal groups = %+v, want single group keyed by member %q", byPrincipal.Groups, member.ID)
	}
	wantNumeric(t, byPrincipal.Groups[0].Amount, "4")

	// An empty window yields a zero total and no groups.
	empty, err := analytics.SpendByOrg(ctx, org.ID, to, to.Add(time.Hour), SpendGroupByAgent)
	if err != nil {
		t.Fatalf("SpendByOrg(empty window): %v", err)
	}
	wantNumeric(t, empty.Total, "0")
	if len(empty.Groups) != 0 || empty.Currency != "USD" {
		t.Fatalf("empty window = %+v, want 0 groups and USD", empty)
	}

	// An unknown group_by is rejected without touching the DB.
	if _, err := analytics.SpendByOrg(ctx, org.ID, from, to, "region"); err == nil {
		t.Fatal("SpendByOrg(invalid group_by) = nil error, want rejection")
	}
}

func TestIntegrationBudgetUtilization(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	budgets := NewBudgetStore(pool)
	rules := NewBudgetRuleStore(pool)

	// budget-a: a monthly rule with a hard cap. budget-b: a daily rule with no
	// hard cap (NULL limit). budget-c: no rules, so it contributes no rows.
	bA, err := budgets.Create(ctx, org.ID, "budget-a", "USD")
	if err != nil {
		t.Fatalf("create budget-a: %v", err)
	}
	if _, err := rules.Create(ctx, CreateBudgetRuleParams{
		BudgetID: bA.ID, Scope: BudgetScopeOrganization, Window: BudgetWindowMonthly, HardCap: mustNumeric(t, "100"),
	}); err != nil {
		t.Fatalf("create monthly rule: %v", err)
	}
	bB, err := budgets.Create(ctx, org.ID, "budget-b", "EUR")
	if err != nil {
		t.Fatalf("create budget-b: %v", err)
	}
	if _, err := rules.Create(ctx, CreateBudgetRuleParams{
		BudgetID: bB.ID, Scope: BudgetScopeOrganization, Window: BudgetWindowDaily,
	}); err != nil {
		t.Fatalf("create daily rule: %v", err)
	}
	if _, err := budgets.Create(ctx, org.ID, "budget-c", "USD"); err != nil {
		t.Fatalf("create budget-c: %v", err)
	}

	now := time.Now().UTC()
	// One event in the current window (counts for both the daily and monthly
	// rule) and one far in the past (counts for neither).
	insertUsage(t, ctx, pool, org.ID, nil, nil, "10", 0, 0, now)
	insertUsage(t, ctx, pool, org.ID, nil, nil, "500", 0, 0, now.AddDate(0, -2, 0))

	usage, err := NewAnalyticsStore(pool).BudgetUtilization(ctx, org.ID, now)
	if err != nil {
		t.Fatalf("BudgetUtilization: %v", err)
	}
	// One row per rule; budget-c (no rules) is absent. Ordered by budget name.
	if len(usage) != 2 {
		t.Fatalf("usage = %+v, want 2 rows (budget-a, budget-b)", usage)
	}

	a := usage[0]
	if a.BudgetID != bA.ID || a.Name != "budget-a" || a.Currency != "USD" || a.Period != BudgetWindowMonthly {
		t.Fatalf("row a = %+v, want budget-a monthly USD", a)
	}
	wantNumeric(t, a.Limit, "100")
	wantNumeric(t, a.Spent, "10")

	b := usage[1]
	if b.BudgetID != bB.ID || b.Name != "budget-b" || b.Currency != "EUR" || b.Period != BudgetWindowDaily {
		t.Fatalf("row b = %+v, want budget-b daily EUR", b)
	}
	if b.Limit.Valid {
		t.Fatalf("row b Limit = %+v, want NULL (no hard cap)", b.Limit)
	}
	wantNumeric(t, b.Spent, "10")

	// An org with no budgets yields an empty (non-error) result.
	other := newOrg(t, ctx, pool)
	none, err := NewAnalyticsStore(pool).BudgetUtilization(ctx, other.ID, now)
	if err != nil {
		t.Fatalf("BudgetUtilization(no budgets): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("BudgetUtilization(no budgets) = %+v, want empty", none)
	}
}
