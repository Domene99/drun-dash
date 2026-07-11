package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These tests exercise the pure value logic that needs no live database: the
// enum Valid() methods and the member-XOR-team pre-check in
// EntitlementStore.Create. Database-touching coverage lives in
// integration_test.go behind the `integration` build tag.

func TestBudgetScopeValid(t *testing.T) {
	valid := []BudgetScope{BudgetScopeOrganization, BudgetScopeTeam, BudgetScopeAgent, BudgetScopeMember}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("BudgetScope(%q).Valid() = false, want true", s)
		}
	}
	invalid := []BudgetScope{"", "org", "Organization", "ORGANIZATION", "user"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("BudgetScope(%q).Valid() = true, want false", s)
		}
	}
}

func TestBudgetWindowValid(t *testing.T) {
	valid := []BudgetWindow{BudgetWindowDaily, BudgetWindowWeekly, BudgetWindowMonthly}
	for _, w := range valid {
		if !w.Valid() {
			t.Errorf("BudgetWindow(%q).Valid() = false, want true", w)
		}
	}
	invalid := []BudgetWindow{"", "day", "Daily", "MONTHLY", "yearly"}
	for _, w := range invalid {
		if w.Valid() {
			t.Errorf("BudgetWindow(%q).Valid() = true, want false", w)
		}
	}
}

// failQuerier is a [Querier] that fails the test if any method is called. It
// proves EntitlementStore.Create rejects a bad principal before issuing SQL.
type failQuerier struct{ t *testing.T }

func (q failQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.t.Helper()
	q.t.Fatal("Query called; validation should have short-circuited before the DB")
	return nil, nil
}

func (q failQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	q.t.Helper()
	q.t.Fatal("QueryRow called; validation should have short-circuited before the DB")
	return nil
}

func (q failQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	q.t.Helper()
	q.t.Fatal("Exec called; validation should have short-circuited before the DB")
	return pgconn.CommandTag{}, nil
}

func TestEntitlementCreateRejectsBadPrincipalBeforeDB(t *testing.T) {
	store := NewEntitlementStore(failQuerier{t: t})
	ctx := context.Background()
	member := "11111111-1111-1111-1111-111111111111"
	team := "22222222-2222-2222-2222-222222222222"

	cases := []struct {
		name   string
		params CreateEntitlementParams
	}{
		{"neither", CreateEntitlementParams{AgentID: "a"}},
		{"both", CreateEntitlementParams{AgentID: "a", MemberID: &member, TeamID: &team}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Create(ctx, tc.params)
			if err == nil {
				t.Fatalf("Create(%s) error = nil, want a validation error", tc.name)
			}
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
				t.Fatalf("Create(%s) error = %v, want a plain validation error (not a store sentinel)", tc.name, err)
			}
			if !strings.Contains(err.Error(), "member_id/team_id") {
				t.Fatalf("Create(%s) error = %q, want it to mention member_id/team_id", tc.name, err)
			}
		})
	}
}
