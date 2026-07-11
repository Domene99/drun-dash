//go:build integration

// Integration coverage for the keyset-paginated ListByOrgPage variants added
// per ADR-0002 §3 (agents, policies, sessions, approval requests, trajectories).
// Each test inserts N rows under a throwaway org, pages through them in
// fixed-size chunks driven by the (created_at, id) cursor, and asserts full
// coverage with no duplicates, no skips, a stable order, and that a zero
// position starts at the beginning. Mirrors the other integration files and
// reuses their shared helpers (requirePool, newOrg, uniq, newAgent, newMember,
// missingUUID); skips when Postgres is unreachable.
package store

import (
	"encoding/json"
	"testing"
	"time"
)

// pageCursor is the (created_at, id) seek position the handler layer would
// carry in its opaque cursor. The zero value starts a scan from the beginning.
type pageCursor struct {
	createdAt time.Time
	id        string
}

// pageAll drives a ListByOrgPage-style fetcher through every row in fixed-size
// chunks and returns the concatenated ids in the order they were seen. fetch
// mirrors the store method's contract: it is asked for chunk+1 rows (lookahead)
// and returns each row's (created_at, id) key. The loop stops when a page comes
// back short, exactly as a handler computing next_cursor would.
func pageAll(t *testing.T, chunk int, fetch func(limit int, after pageCursor) []pageCursor) []string {
	t.Helper()
	var order []string
	var after pageCursor
	for i := 0; ; i++ {
		if i > 10_000 {
			t.Fatal("pageAll did not terminate; cursor is not advancing")
		}
		page := fetch(chunk+1, after)
		if len(page) > chunk+1 {
			t.Fatalf("page returned %d rows, want at most limit+1=%d", len(page), chunk+1)
		}
		last := page
		hasMore := false
		if len(page) == chunk+1 {
			last = page[:chunk] // drop the lookahead row
			hasMore = true
		}
		for _, row := range last {
			order = append(order, row.id)
		}
		if !hasMore {
			break
		}
		after = last[len(last)-1]
	}
	return order
}

// assertContiguous checks that got is exactly want (same elements, same order):
// full coverage, no duplicates, no skips, stable order.
func assertContiguous(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("paged %d ids, want %d", len(got), len(want))
	}
	seen := make(map[string]bool, len(got))
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("id at %d = %s, want %s (order not stable / rows skipped)", i, got[i], want[i])
		}
		if seen[got[i]] {
			t.Fatalf("id %s appeared more than once", got[i])
		}
		seen[got[i]] = true
	}
}

func TestIntegrationAgentsListByOrgPage(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	other := newOrg(t, ctx, pool)
	agents := NewAgentStore(pool)

	const n = 25
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		a, err := agents.Create(ctx, org.ID, uniq("agent"), nil)
		if err != nil {
			t.Fatalf("create agent %d: %v", i, err)
		}
		want = append(want, a.ID)
	}
	// A row in another org must never surface in this org's pages.
	if _, err := agents.Create(ctx, other.ID, uniq("agent"), nil); err != nil {
		t.Fatalf("create other-org agent: %v", err)
	}

	// A zero position starts at the beginning; verify against a single big page.
	first, err := agents.ListByOrgPage(ctx, org.ID, n, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListByOrgPage(zero): %v", err)
	}
	if len(first) != n || first[0].ID != want[0] {
		t.Fatalf("zero-position page = %d rows starting %v, want %d starting %s", len(first), firstID(first), n, want[0])
	}

	for _, chunk := range []int{1, 4, 7, n} {
		got := pageAll(t, chunk, func(limit int, after pageCursor) []pageCursor {
			rows, err := agents.ListByOrgPage(ctx, org.ID, limit, after.createdAt, after.id)
			if err != nil {
				t.Fatalf("ListByOrgPage(chunk=%d): %v", chunk, err)
			}
			return agentCursors(rows)
		})
		assertContiguous(t, got, want)
	}

	// A non-positive limit is a programming error.
	if _, err := agents.ListByOrgPage(ctx, org.ID, 0, time.Time{}, ""); err == nil {
		t.Fatal("ListByOrgPage(limit=0) = nil error, want error")
	}
}

func TestIntegrationPoliciesListByOrgPage(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	other := newOrg(t, ctx, pool)
	policies := NewPolicyStore(pool)

	const n = 25
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p, err := policies.Create(ctx, org.ID, uniq("policy"), nil)
		if err != nil {
			t.Fatalf("create policy %d: %v", i, err)
		}
		want = append(want, p.ID)
	}
	if _, err := policies.Create(ctx, other.ID, uniq("policy"), nil); err != nil {
		t.Fatalf("create other-org policy: %v", err)
	}

	for _, chunk := range []int{1, 4, 7, n} {
		got := pageAll(t, chunk, func(limit int, after pageCursor) []pageCursor {
			rows, err := policies.ListByOrgPage(ctx, org.ID, limit, after.createdAt, after.id)
			if err != nil {
				t.Fatalf("ListByOrgPage(chunk=%d): %v", chunk, err)
			}
			return policyCursors(rows)
		})
		assertContiguous(t, got, want)
	}

	if _, err := policies.ListByOrgPage(ctx, org.ID, -1, time.Time{}, ""); err == nil {
		t.Fatal("ListByOrgPage(limit=-1) = nil error, want error")
	}
}

func TestIntegrationSessionsListByOrgPage(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	other := newOrg(t, ctx, pool)
	agent := newAgent(t, ctx, pool, org.ID)
	otherAgent := newAgent(t, ctx, pool, other.ID)
	sessions := NewSessionStore(pool)

	const n = 25
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		s, err := sessions.Create(ctx, CreateSessionParams{OrgID: org.ID, AgentID: agent.ID, FanOutCeiling: 1})
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
		want = append(want, s.ID)
	}
	if _, err := sessions.Create(ctx, CreateSessionParams{OrgID: other.ID, AgentID: otherAgent.ID, FanOutCeiling: 1}); err != nil {
		t.Fatalf("create other-org session: %v", err)
	}

	for _, chunk := range []int{1, 4, 7, n} {
		got := pageAll(t, chunk, func(limit int, after pageCursor) []pageCursor {
			rows, err := sessions.ListByOrgPage(ctx, org.ID, limit, after.createdAt, after.id)
			if err != nil {
				t.Fatalf("ListByOrgPage(chunk=%d): %v", chunk, err)
			}
			return sessionCursors(rows)
		})
		assertContiguous(t, got, want)
	}

	if _, err := sessions.ListByOrgPage(ctx, org.ID, 0, time.Time{}, ""); err == nil {
		t.Fatal("ListByOrgPage(limit=0) = nil error, want error")
	}
}

func TestIntegrationApprovalRequestsListByOrgPage(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	other := newOrg(t, ctx, pool)
	agent := newAgent(t, ctx, pool, org.ID)
	otherAgent := newAgent(t, ctx, pool, other.ID)
	session, err := NewSessionStore(pool).Create(ctx, CreateSessionParams{OrgID: org.ID, AgentID: agent.ID, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	otherSession, err := NewSessionStore(pool).Create(ctx, CreateSessionParams{OrgID: other.ID, AgentID: otherAgent.ID, FanOutCeiling: 1})
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}
	approvals := NewApprovalRequestStore(pool)

	const n = 25
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		r, err := approvals.Create(ctx, CreateApprovalRequestParams{OrgID: org.ID, SessionID: session.ID, Action: "tool.exec"})
		if err != nil {
			t.Fatalf("create approval %d: %v", i, err)
		}
		want = append(want, r.ID)
	}
	if _, err := approvals.Create(ctx, CreateApprovalRequestParams{OrgID: other.ID, SessionID: otherSession.ID, Action: "tool.exec"}); err != nil {
		t.Fatalf("create other-org approval: %v", err)
	}

	for _, chunk := range []int{1, 4, 7, n} {
		got := pageAll(t, chunk, func(limit int, after pageCursor) []pageCursor {
			rows, err := approvals.ListByOrgPage(ctx, org.ID, limit, after.createdAt, after.id)
			if err != nil {
				t.Fatalf("ListByOrgPage(chunk=%d): %v", chunk, err)
			}
			return approvalCursors(rows)
		})
		assertContiguous(t, got, want)
	}

	if _, err := approvals.ListByOrgPage(ctx, org.ID, 0, time.Time{}, ""); err == nil {
		t.Fatal("ListByOrgPage(limit=0) = nil error, want error")
	}
}

func TestIntegrationTrajectoriesListByOrgPage(t *testing.T) {
	pool, ctx := requirePool(t)
	org := newOrg(t, ctx, pool)
	other := newOrg(t, ctx, pool)
	trajectories := NewTrajectoryStore(pool)

	const n = 25
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// Backfill started_at to a spread of historical times to prove the page
		// key is created_at (insertion order), not the mutable started_at.
		started := time.Now().Add(-time.Duration(i) * time.Hour).UTC().Truncate(time.Microsecond)
		tr, err := trajectories.Create(ctx, CreateTrajectoryParams{
			OrgID: org.ID, MetadataJSON: json.RawMessage(`{"i":` + itoa(i) + `}`), StartedAt: &started,
		})
		if err != nil {
			t.Fatalf("create trajectory %d: %v", i, err)
		}
		want = append(want, tr.ID)
	}
	if _, err := trajectories.Create(ctx, CreateTrajectoryParams{OrgID: other.ID}); err != nil {
		t.Fatalf("create other-org trajectory: %v", err)
	}

	for _, chunk := range []int{1, 4, 7, n} {
		got := pageAll(t, chunk, func(limit int, after pageCursor) []pageCursor {
			rows, err := trajectories.ListByOrgPage(ctx, org.ID, limit, after.createdAt, after.id)
			if err != nil {
				t.Fatalf("ListByOrgPage(chunk=%d): %v", chunk, err)
			}
			return trajectoryCursors(rows)
		})
		assertContiguous(t, got, want)
	}

	if _, err := trajectories.ListByOrgPage(ctx, org.ID, 0, time.Time{}, ""); err == nil {
		t.Fatal("ListByOrgPage(limit=0) = nil error, want error")
	}
}

// --- small helpers to project rows onto their (created_at, id) page keys ---

func agentCursors(rows []Agent) []pageCursor {
	out := make([]pageCursor, len(rows))
	for i, r := range rows {
		out[i] = pageCursor{createdAt: r.CreatedAt, id: r.ID}
	}
	return out
}

func policyCursors(rows []Policy) []pageCursor {
	out := make([]pageCursor, len(rows))
	for i, r := range rows {
		out[i] = pageCursor{createdAt: r.CreatedAt, id: r.ID}
	}
	return out
}

func sessionCursors(rows []Session) []pageCursor {
	out := make([]pageCursor, len(rows))
	for i, r := range rows {
		out[i] = pageCursor{createdAt: r.CreatedAt, id: r.ID}
	}
	return out
}

func approvalCursors(rows []ApprovalRequest) []pageCursor {
	out := make([]pageCursor, len(rows))
	for i, r := range rows {
		out[i] = pageCursor{createdAt: r.CreatedAt, id: r.ID}
	}
	return out
}

func trajectoryCursors(rows []Trajectory) []pageCursor {
	out := make([]pageCursor, len(rows))
	for i, r := range rows {
		out[i] = pageCursor{createdAt: r.CreatedAt, id: r.ID}
	}
	return out
}

func firstID(rows []Agent) string {
	if len(rows) == 0 {
		return "<empty>"
	}
	return rows[0].ID
}

// itoa avoids importing strconv just for the trajectory metadata payload.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
