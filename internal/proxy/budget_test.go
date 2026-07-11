package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Domene99/drun-dash/internal/store"
)

// numericFromString builds a pgtype.Numeric money value from its decimal string
// form, mirroring the helper in internal/httpapi so budget fixtures carry exact
// numeric(20,6) amounts.
func numericFromString(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		t.Fatalf("scan numeric %q: %v", s, err)
	}
	return n
}

// budgetRow builds a budget-utilisation row with a hard cap of limit and the
// given spend, both as decimal strings.
func budgetRow(t *testing.T, name, limit, spent string) store.BudgetUsageRow {
	t.Helper()
	return store.BudgetUsageRow{
		BudgetID: "budget-" + name,
		Name:     name,
		Currency: "USD",
		Limit:    numericFromString(t, limit),
		Spent:    numericFromString(t, spent),
		Period:   store.BudgetWindowMonthly,
	}
}

// blockedBroker builds a broker whose upstream and usage writer both fail the
// test if reached, so a call that should be blocked by a budget cap but is
// nonetheless forwarded or metered is caught.
func blockedBroker(t *testing.T) (*broker, *fakeUsage) {
	t.Helper()
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request was forwarded upstream despite a hit budget cap")
		return nil, nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	usage := &fakeUsage{}
	b.usage = usage
	return b, usage
}

// TestBrokerBlocksAtBudgetCap asserts that when the org has a budget whose
// current-window spend exceeds its hard cap, the call is refused with 429
// budget_exceeded, the upstream is never contacted, and nothing is metered.
func TestBrokerBlocksAtBudgetCap(t *testing.T) {
	b, usage := blockedBroker(t)
	b.budgets = fakeBudgets{rows: []store.BudgetUsageRow{
		budgetRow(t, "engineering", "100.000000", "150.000000"),
	}}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	assertErrorCode(t, rec, budgetExceededCode)
	if body := rec.Body.String(); !strings.Contains(body, "engineering") {
		t.Errorf("error body = %q, want it to name the budget", body)
	}
	if events := usage.recorded(); len(events) != 0 {
		t.Fatalf("recorded %d usage events, want 0 for a blocked call", len(events))
	}
}

// TestBrokerBlocksAtExactBudgetCap asserts the cap is hit at exact equality
// (Spent == Limit), matching the design's Spent >= Limit rule.
func TestBrokerBlocksAtExactBudgetCap(t *testing.T) {
	b, _ := blockedBroker(t)
	b.budgets = fakeBudgets{rows: []store.BudgetUsageRow{
		budgetRow(t, "exact", "50.000000", "50.000000"),
	}}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	assertErrorCode(t, rec, budgetExceededCode)
}

// TestBrokerBlocksWhenAnyBudgetHit asserts that with several budgets, a single
// hit cap blocks the call even when the others are well under.
func TestBrokerBlocksWhenAnyBudgetHit(t *testing.T) {
	b, _ := blockedBroker(t)
	b.budgets = fakeBudgets{rows: []store.BudgetUsageRow{
		budgetRow(t, "under", "1000.000000", "10.000000"),
		budgetRow(t, "over", "100.000000", "100.000001"),
	}}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if body := rec.Body.String(); !strings.Contains(body, "over") {
		t.Errorf("error body = %q, want it to name the over-cap budget", body)
	}
}

// TestBrokerAllowsUnderBudgetCap asserts a call whose org is under all caps
// proceeds exactly as batch 12: it forwards to the upstream and is metered.
func TestBrokerAllowsUnderBudgetCap(t *testing.T) {
	forwarded := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return jsonResponse(`{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":2}}`), nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	usage := &fakeUsage{}
	b.usage = usage
	b.budgets = fakeBudgets{rows: []store.BudgetUsageRow{
		budgetRow(t, "engineering", "100.000000", "99.999999"),
	}}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if !forwarded {
		t.Fatal("under-cap call was not forwarded upstream")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if events := usage.recorded(); len(events) != 1 {
		t.Fatalf("recorded %d usage events, want 1 for an under-cap call", len(events))
	}
}

// TestBrokerAllowsWithNoBudgets asserts an org with no budgets configured is
// never blocked (the default test fixture already exercises this, made explicit
// here).
func TestBrokerAllowsWithNoBudgets(t *testing.T) {
	forwarded := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return jsonResponse(`{"ok":true}`), nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	b.budgets = fakeBudgets{} // no rows

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if !forwarded {
		t.Fatal("call with no budgets configured was not forwarded")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestBrokerIgnoresRuleWithoutHardCap asserts a budget rule with no hard cap (an
// invalid/NULL Limit) imposes no ceiling: the call proceeds even though spend is
// high, since there is nothing to exceed.
func TestBrokerIgnoresRuleWithoutHardCap(t *testing.T) {
	forwarded := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded = true
		return jsonResponse(`{"ok":true}`), nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	b.budgets = fakeBudgets{rows: []store.BudgetUsageRow{
		{
			BudgetID: "budget-nocap",
			Name:     "no-cap",
			Currency: "USD",
			Limit:    pgtype.Numeric{}, // invalid == no hard cap
			Spent:    numericFromString(t, "9999.000000"),
			Period:   store.BudgetWindowMonthly,
		},
	}}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if !forwarded {
		t.Fatal("call was blocked despite the rule having no hard cap")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestBrokerBudgetLookupErrorIs500 asserts the budget check fails closed: a
// utilisation-lookup error yields 500 and does not forward, so the proxy never
// spends when it cannot prove the org is under cap.
func TestBrokerBudgetLookupErrorIs500(t *testing.T) {
	b, _ := blockedBroker(t)
	b.budgets = fakeBudgets{err: context.DeadlineExceeded}

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertErrorCode(t, rec, "internal_error")
}

// TestBrokerBudgetCheckIsOrgScoped asserts the budget check resolves utilisation
// for the session's org, so caps are enforced against the calling org's spend.
func TestBrokerBudgetCheckIsOrgScoped(t *testing.T) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"ok":true}`), nil
	})
	b := newTestBroker(t, "https://upstream.test", rt)
	spy := &orgSpyBudgets{}
	b.budgets = spy

	req := authorizedRequest(http.MethodPost, "/v1/chat/completions", "{}")
	rec := httptest.NewRecorder()
	b.chatCompletions(rec, req)

	if spy.orgID != testOrgID {
		t.Errorf("BudgetUtilization org_id = %q, want the session's org %q", spy.orgID, testOrgID)
	}
}

// orgSpyBudgets records the orgID BudgetUtilization is called with and reports
// no budgets (under cap).
type orgSpyBudgets struct{ orgID string }

func (s *orgSpyBudgets) BudgetUtilization(_ context.Context, orgID string, _ time.Time) ([]store.BudgetUsageRow, error) {
	s.orgID = orgID
	return nil, nil
}

// TestCheckBudget covers the budget comparison directly, including the boundary
// and the no-hard-cap skip.
func TestCheckBudget(t *testing.T) {
	over := budgetRow(t, "over", "100.000000", "100.000001")
	exact := budgetRow(t, "exact", "100.000000", "100.000000")
	under := budgetRow(t, "under", "100.000000", "99.999999")
	nocap := store.BudgetUsageRow{Name: "nocap", Limit: pgtype.Numeric{}, Spent: numericFromString(t, "9999")}

	cases := []struct {
		name    string
		rows    []store.BudgetUsageRow
		wantHit bool
		wantRow string
	}{
		{"over", []store.BudgetUsageRow{over}, true, "over"},
		{"exact", []store.BudgetUsageRow{exact}, true, "exact"},
		{"under", []store.BudgetUsageRow{under}, false, ""},
		{"no hard cap", []store.BudgetUsageRow{nocap}, false, ""},
		{"none", nil, false, ""},
		{"first hit wins", []store.BudgetUsageRow{under, over}, true, "over"},
		{"skip nocap then hit", []store.BudgetUsageRow{nocap, over}, true, "over"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row, hit, err := checkBudget(context.Background(), fakeBudgets{rows: c.rows}, testOrgID, time.Now())
			if err != nil {
				t.Fatalf("checkBudget: %v", err)
			}
			if hit != c.wantHit {
				t.Fatalf("hit = %v, want %v", hit, c.wantHit)
			}
			if hit && row.Name != c.wantRow {
				t.Errorf("hit row = %q, want %q", row.Name, c.wantRow)
			}
		})
	}
}

// jsonResponse builds a 200 application/json upstream response body.
func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
