package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakeAnalyticsStore is an in-memory analyticsStore: it returns canned
// summaries/rows and records the arguments each method was called with, so the
// handler tests can assert on org scoping and window forwarding without a
// database.
type fakeAnalyticsStore struct {
	summary   store.SpendSummary
	spendErr  error
	budgets   []store.BudgetUsageRow
	budgetErr error

	// Captured call arguments.
	gotSpendOrg     string
	gotSpendFrom    time.Time
	gotSpendTo      time.Time
	gotSpendGroupBy string
	gotBudgetOrg    string
	gotBudgetNow    time.Time
}

func (f *fakeAnalyticsStore) SpendByOrg(_ context.Context, orgID string, from, to time.Time, groupBy string) (store.SpendSummary, error) {
	f.gotSpendOrg, f.gotSpendFrom, f.gotSpendTo, f.gotSpendGroupBy = orgID, from, to, groupBy
	return f.summary, f.spendErr
}

func (f *fakeAnalyticsStore) BudgetUtilization(_ context.Context, orgID string, now time.Time) ([]store.BudgetUsageRow, error) {
	f.gotBudgetOrg, f.gotBudgetNow = orgID, now
	return f.budgets, f.budgetErr
}

// setAnalyticsStore points the newAnalyticsStore seam at s for the duration of
// the test, restoring the production factory afterwards. Because it mutates a
// package-level var, callers must not run in parallel.
func setAnalyticsStore(t *testing.T, s analyticsStore) {
	t.Helper()
	orig := newAnalyticsStore
	newAnalyticsStore = func(store.Querier) analyticsStore { return s }
	t.Cleanup(func() { newAnalyticsStore = orig })
}

func analyticsHarness(t *testing.T, fake analyticsStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAnalyticsStore(t, fake)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

// numericFromString builds a valid pgtype.Numeric from a decimal string, for
// seeding fake store money values.
func numericFromString(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		t.Fatalf("scan numeric %q: %v", s, err)
	}
	return n
}

// TestQuerySpend proves the happy path: from/to/group_by are parsed and
// forwarded to the store scoped to the caller's org, and the store's precise
// numerics are mapped to JSON numbers in the SpendReport.
func TestQuerySpend(t *testing.T) {
	fake := &fakeAnalyticsStore{summary: store.SpendSummary{
		Total:    numericFromString(t, "12.500000"),
		Currency: "USD",
		Groups: []store.SpendGroupRow{
			{Key: "agt_1", Amount: numericFromString(t, "10.000000"), Tokens: 4000},
			{Key: "", Amount: numericFromString(t, "2.500000"), Tokens: 1000},
		},
	}}
	h, hdr := analyticsHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet,
		"/analytics/spend?from=2026-07-01T00:00:00Z&to=2026-07-11T00:00:00Z&group_by=agent", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	if fake.gotSpendOrg != "org-1" {
		t.Errorf("spend org = %q, want org-1", fake.gotSpendOrg)
	}
	wantFrom := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	if !fake.gotSpendFrom.Equal(wantFrom) || !fake.gotSpendTo.Equal(wantTo) {
		t.Errorf("window = [%v, %v), want [%v, %v)", fake.gotSpendFrom, fake.gotSpendTo, wantFrom, wantTo)
	}
	if fake.gotSpendGroupBy != "agent" {
		t.Errorf("group_by = %q, want agent", fake.gotSpendGroupBy)
	}

	var body apiSpendReport
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 12.5 || body.Currency != "USD" {
		t.Errorf("total/currency = %v/%q, want 12.5/USD", body.Total, body.Currency)
	}
	if len(body.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(body.Groups))
	}
	if body.Groups[0].Key != "agt_1" || body.Groups[0].Amount != 10 || body.Groups[0].Tokens != 4000 {
		t.Errorf("group[0] = %+v, want {agt_1 10 4000}", body.Groups[0])
	}
}

// TestQuerySpendDefaultGroupBy proves an absent group_by defaults to agent.
func TestQuerySpendDefaultGroupBy(t *testing.T) {
	fake := &fakeAnalyticsStore{summary: store.SpendSummary{Total: numericFromString(t, "0"), Currency: "USD"}}
	h, hdr := analyticsHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet,
		"/analytics/spend?from=2026-07-01T00:00:00Z&to=2026-07-11T00:00:00Z", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotSpendGroupBy != "agent" {
		t.Errorf("group_by = %q, want agent (default)", fake.gotSpendGroupBy)
	}
	// Empty groups must serialise as [] (never null).
	if _, ok := rawKeys(t, rec.Body.Bytes())["groups"]; !ok {
		t.Error("response missing groups key")
	}
	var body apiSpendReport
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Groups == nil {
		t.Error("groups = null, want []")
	}
}

func TestQuerySpendBadParams(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"missing from", "/analytics/spend?to=2026-07-11T00:00:00Z"},
		{"missing to", "/analytics/spend?from=2026-07-01T00:00:00Z"},
		{"bad from", "/analytics/spend?from=nope&to=2026-07-11T00:00:00Z"},
		{"bad group_by", "/analytics/spend?from=2026-07-01T00:00:00Z&to=2026-07-11T00:00:00Z&group_by=team"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAnalyticsStore{}
			h, hdr := analyticsHarness(t, fake, "org-1")
			rec := doJSON(t, h, http.MethodGet, tc.path, "", hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			// A validation failure must short-circuit before the store call.
			if fake.gotSpendOrg != "" {
				t.Error("store was called despite invalid params")
			}
		})
	}
}

func TestQuerySpendStoreError(t *testing.T) {
	fake := &fakeAnalyticsStore{spendErr: context.DeadlineExceeded}
	h, hdr := analyticsHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodGet,
		"/analytics/spend?from=2026-07-01T00:00:00Z&to=2026-07-11T00:00:00Z", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestListBudgets proves the happy path maps utilisation rows to the BudgetList
// and that a rule with no hard cap (invalid limit) surfaces limit 0.
func TestListBudgets(t *testing.T) {
	fake := &fakeAnalyticsStore{budgets: []store.BudgetUsageRow{
		{
			BudgetID: "bud_1", Name: "eng", Currency: "USD",
			Limit: numericFromString(t, "100.000000"), Spent: numericFromString(t, "42.250000"),
			Period: store.BudgetWindowMonthly,
		},
		{
			BudgetID: "bud_2", Name: "uncapped", Currency: "USD",
			Limit: pgtype.Numeric{}, Spent: numericFromString(t, "5.000000"),
			Period: store.BudgetWindowDaily,
		},
	}}
	h, hdr := analyticsHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/analytics/budgets", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotBudgetOrg != "org-1" {
		t.Errorf("budget org = %q, want org-1", fake.gotBudgetOrg)
	}
	if fake.gotBudgetNow.IsZero() {
		t.Error("budget now = zero, want the current time forwarded")
	}

	var body apiBudgetList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[0].Limit != 100 || body.Items[0].Spent != 42.25 || body.Items[0].Period != "monthly" {
		t.Errorf("item[0] = %+v, want limit 100 spent 42.25 monthly", body.Items[0])
	}
	if body.Items[1].Limit != 0 || body.Items[1].Period != "daily" {
		t.Errorf("item[1] = %+v, want limit 0 (uncapped) daily", body.Items[1])
	}
}

// TestListBudgetsEmpty proves items serialises as [] (never null) when the org
// has no budgets.
func TestListBudgetsEmpty(t *testing.T) {
	fake := &fakeAnalyticsStore{budgets: nil}
	h, hdr := analyticsHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/analytics/budgets", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if items := rawKeys(t, rec.Body.Bytes())["items"]; string(items) != "[]" {
		t.Errorf("items = %s, want []", items)
	}
}

func TestListBudgetsStoreError(t *testing.T) {
	fake := &fakeAnalyticsStore{budgetErr: context.DeadlineExceeded}
	h, hdr := analyticsHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/analytics/budgets", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAnalyticsRoutesRequireAuth proves the analytics routes sit behind
// RequireAuth: a request with no token is a 401 and never reaches the store.
func TestAnalyticsRoutesRequireAuth(t *testing.T) {
	paths := []string{
		"/analytics/spend?from=2026-07-01T00:00:00Z&to=2026-07-11T00:00:00Z",
		"/analytics/budgets",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setAnalyticsStore(t, &fakeAnalyticsStore{})
			h := mountRouter(deps)
			rec := doJSON(t, h, http.MethodGet, p, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}
