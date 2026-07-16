package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These tests exercise the AccessResolver's policy-centric decision logic
// (ADR-0010) with an in-memory Querier that answers each of the resolver's
// boolean sub-queries from canned data — no database. They prove the Go-level
// predicate composition (agents.run gate → all_agents short-circuit → policy
// check) and its fail-closed error handling. The SQL joins themselves (the
// reachable-roles CTE, the governing-policy resolution, team-role reachability)
// are covered by rbac_integration_test.go behind the `integration` tag.

// fakeRow is a single-column pgx.Row returning a canned bool (or a scan error).
type fakeRow struct {
	val bool
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 1 {
		if p, ok := dest[0].(*bool); ok {
			*p = r.val
		}
	}
	return nil
}

// resolverQuerier is a Querier that routes the resolver's QueryRow calls by SQL
// shape: the permission EXISTS join (role_permissions), the all-agents EXISTS
// join (r.all_agents), and the governing-policy predicate (policy_roles). Each
// returns its canned bool; policyErr lets a test simulate the agent-missing /
// error path. It records which sub-queries ran so a test can assert
// short-circuiting. Query and Exec are not used by these paths and fail loudly.
type resolverQuerier struct {
	hasRun    bool
	allAgents bool
	policyOK  bool
	hasRunErr error
	policyErr error

	hasRunCalls    int
	allAgentsCalls int
	policyCalls    int
}

func (q *resolverQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "role_permissions"):
		q.hasRunCalls++
		return fakeRow{val: q.hasRun, err: q.hasRunErr}
	case strings.Contains(sql, "r.all_agents"):
		q.allAgentsCalls++
		return fakeRow{val: q.allAgents}
	case strings.Contains(sql, "policy_roles"):
		q.policyCalls++
		return fakeRow{val: q.policyOK, err: q.policyErr}
	}
	return fakeRow{err: errors.New("resolverQuerier: unexpected QueryRow: " + sql)}
}

func (q *resolverQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("resolverQuerier: unexpected Query: " + sql)
}

func (q *resolverQuerier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("resolverQuerier: unexpected Exec: " + sql)
}

func TestAuthorizeRunPredicate(t *testing.T) {
	ctx := context.Background()

	t.Run("no agents.run denies without consulting access", func(t *testing.T) {
		q := &resolverQuerier{hasRun: false, allAgents: true, policyOK: true}
		grant, ok, err := NewAccessResolver(q).AuthorizeRun(ctx, "m1", "a1")
		if err != nil || ok || grant.AllAgents {
			t.Fatalf("AuthorizeRun = (%+v, %v, %v); want ({}, false, nil)", grant, ok, err)
		}
		if q.allAgentsCalls != 0 || q.policyCalls != 0 {
			t.Fatalf("access consulted despite missing agents.run: allAgents=%d policy=%d", q.allAgentsCalls, q.policyCalls)
		}
	})

	t.Run("all_agents authorizes any agent without a policy check", func(t *testing.T) {
		q := &resolverQuerier{hasRun: true, allAgents: true, policyOK: false}
		grant, ok, err := NewAccessResolver(q).AuthorizeRun(ctx, "m1", "a1")
		if err != nil || !ok || !grant.AllAgents {
			t.Fatalf("AuthorizeRun = (%+v, %v, %v); want ({AllAgents:true}, true, nil)", grant, ok, err)
		}
		if q.policyCalls != 0 {
			t.Fatalf("policy consulted despite all_agents: %d", q.policyCalls)
		}
	})

	t.Run("policy permits the member authorizes", func(t *testing.T) {
		q := &resolverQuerier{hasRun: true, allAgents: false, policyOK: true}
		grant, ok, err := NewAccessResolver(q).AuthorizeRun(ctx, "m1", "a1")
		if err != nil || !ok {
			t.Fatalf("AuthorizeRun = (%+v, %v, %v); want (_, true, nil)", grant, ok, err)
		}
		if grant.AllAgents {
			t.Fatal("grant.AllAgents = true, want false (reached via policy, not all_agents)")
		}
		if q.policyCalls != 1 {
			t.Fatalf("policy consulted %d times, want 1", q.policyCalls)
		}
	})

	t.Run("policy without the member denies", func(t *testing.T) {
		q := &resolverQuerier{hasRun: true, allAgents: false, policyOK: false}
		_, ok, err := NewAccessResolver(q).AuthorizeRun(ctx, "m1", "a1")
		if err != nil || ok {
			t.Fatalf("AuthorizeRun = (_, %v, %v); want (false, nil)", ok, err)
		}
	})

	t.Run("permission error propagates fail-closed", func(t *testing.T) {
		q := &resolverQuerier{hasRunErr: context.DeadlineExceeded}
		_, ok, err := NewAccessResolver(q).AuthorizeRun(ctx, "m1", "a1")
		if err == nil || ok {
			t.Fatalf("AuthorizeRun = (_, %v, %v); want (false, error)", ok, err)
		}
	})

	t.Run("missing agent denies (no policy row)", func(t *testing.T) {
		q := &resolverQuerier{hasRun: true, allAgents: false, policyErr: pgx.ErrNoRows}
		_, ok, err := NewAccessResolver(q).AuthorizeRun(ctx, "m1", "missing")
		if err != nil || ok {
			t.Fatalf("AuthorizeRun(missing agent) = (_, %v, %v); want (false, nil)", ok, err)
		}
	})
}

// TestListAccessibleAgentIDsNoRun proves the accessible set is empty (and no
// agent query runs) for a member lacking agents.run.
func TestListAccessibleAgentIDsNoRun(t *testing.T) {
	q := &resolverQuerier{hasRun: false}
	ids, err := NewAccessResolver(q).ListAccessibleAgentIDs(context.Background(), "m1", "org-1")
	if err != nil || ids != nil {
		t.Fatalf("ListAccessibleAgentIDs(no run) = (%v, %v); want (nil, nil)", ids, err)
	}
	if q.allAgentsCalls != 0 {
		t.Fatalf("all_agents consulted despite missing agents.run: %d", q.allAgentsCalls)
	}
}
