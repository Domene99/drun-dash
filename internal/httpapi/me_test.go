package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// mapAgentStore is an agentStore backed by an id→agent map: GetByID looks the
// id up (missing → store.ErrNotFound), which is all /me/agents needs. The other
// interface methods are unused no-ops. A set getErr overrides the map so the
// handler's error path can be exercised.
type mapAgentStore struct {
	agents map[string]store.Agent
	getErr error
}

func (m mapAgentStore) Create(context.Context, string, string, *string) (store.Agent, error) {
	return store.Agent{}, nil
}

func (m mapAgentStore) GetByID(_ context.Context, id string) (store.Agent, error) {
	if m.getErr != nil {
		return store.Agent{}, m.getErr
	}
	a, ok := m.agents[id]
	if !ok {
		return store.Agent{}, store.ErrNotFound
	}
	return a, nil
}

func (m mapAgentStore) ListByOrgPage(context.Context, string, int, time.Time, string) ([]store.Agent, error) {
	return nil, nil
}

func (m mapAgentStore) Update(context.Context, string, string, *string) (store.Agent, error) {
	return store.Agent{}, nil
}

func (m mapAgentStore) Delete(context.Context, string) error { return nil }

func (m mapAgentStore) SetCurrentVersion(context.Context, string, int) (store.Agent, error) {
	return store.Agent{}, nil
}

// meHarness wires a Deps with a shared issuer/verifier key, points the resolver
// and agent-store seams /me/agents reads through at the supplied fakes, mounts
// the router, and returns it with an Authorization header for member-1 in orgID
// (agentToken mints member_id "member-1").
func meHarness(t *testing.T, res accessResolver, agents agentStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, nil, fakeMembers{})
	setResolver(t, res)
	setAgentStore(t, agents)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

// TestListMyAgentsVisibleSet proves /me/agents returns the agents the resolver
// reports as accessible under the policy-centric model (ADR-0010), preserving
// the resolver's (created_at, id) order, dropping a defensively cross-org agent,
// and skipping an id whose agent no longer exists.
func TestListMyAgentsVisibleSet(t *testing.T) {
	created := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	res := &fakeResolver{accessible: []string{"a1", "a2", "a3", "gone"}}
	agents := mapAgentStore{agents: map[string]store.Agent{
		"a1": storeAgent("a1", "org-1", "one", nil, 1, created),
		"a2": storeAgent("a2", "org-1", "two", strptr("second"), 2, created),
		"a3": storeAgent("a3", "org-2", "cross-org", nil, 1, created),
		// "gone" intentionally absent → GetByID returns ErrNotFound.
	}}

	h, hdr := meHarness(t, res, agents, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// The resolver is asked for member-1's accessible set in org-1.
	if res.gotMemberID != "member-1" || res.gotOrgID != "org-1" {
		t.Errorf("resolver called with (%q, %q), want (member-1, org-1)", res.gotMemberID, res.gotOrgID)
	}

	var body apiAgentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// a1 then a2 (resolver order); a3 dropped as cross-org; gone skipped.
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2 (%+v)", len(body.Items), body.Items)
	}
	if body.Items[0].ID != "a1" || body.Items[1].ID != "a2" {
		t.Errorf("items order = [%s %s], want [a1 a2]", body.Items[0].ID, body.Items[1].ID)
	}
	if body.Items[1].Description == nil || *body.Items[1].Description != "second" {
		t.Errorf("a2 description mapped wrong: %+v", body.Items[1])
	}
	// No pagination: the cursor is always null.
	if body.NextCursor != nil {
		t.Errorf("next_cursor = %q, want null", *body.NextCursor)
	}
}

// TestListMyAgentsEmpty proves items serialises as [] (never null) when the
// resolver reports no accessible agents.
func TestListMyAgentsEmpty(t *testing.T) {
	res := &fakeResolver{accessible: nil}
	agents := mapAgentStore{agents: map[string]store.Agent{}}

	h, hdr := meHarness(t, res, agents, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !containsItemsArray(got) {
		t.Errorf("body = %s, want items:[] present", got)
	}
	var body apiAgentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 0 {
		t.Errorf("items = %d, want 0", len(body.Items))
	}
	if body.NextCursor != nil {
		t.Errorf("next_cursor = %q, want null", *body.NextCursor)
	}
}

// TestListMyAgentsRequiresAuth proves the route is behind RequireAuth: a request
// with no token is 401, not 404 — the route is mounted.
func TestListMyAgentsRequiresAuth(t *testing.T) {
	h, _ := meHarness(t, &fakeResolver{}, mapAgentStore{}, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route mounted, missing token)", rec.Code)
	}
}

// TestListMyAgentsResolverError proves a resolver failure resolving the visible
// set surfaces as a 500 rather than a partial list.
func TestListMyAgentsResolverError(t *testing.T) {
	res := &fakeResolver{accessibleErr: errors.New("boom")}
	h, hdr := meHarness(t, res, mapAgentStore{}, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestListMyAgentsSkipsDeletedAgent proves an accessible id pointing at an agent
// that no longer exists (GetByID → not found) is skipped rather than failing the
// list.
func TestListMyAgentsSkipsDeletedAgent(t *testing.T) {
	created := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	res := &fakeResolver{accessible: []string{"gone", "a1"}}
	agents := mapAgentStore{agents: map[string]store.Agent{
		"a1": storeAgent("a1", "org-1", "one", nil, 1, created),
	}}

	h, hdr := meHarness(t, res, agents, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body apiAgentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != "a1" {
		t.Fatalf("items = %+v, want just a1 (missing agent skipped)", body.Items)
	}
}
