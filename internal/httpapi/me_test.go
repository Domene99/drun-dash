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

// fakeMeEntitlements is an in-memory meEntitlementReader keyed by principal, so
// the /me/agents handler can be exercised without a database. ListByMember and
// ListByTeam read their respective maps; a set error is returned instead.
type fakeMeEntitlements struct {
	byMember  map[string][]store.Entitlement
	byTeam    map[string][]store.Entitlement
	memberErr error
	teamErr   error
}

func (f fakeMeEntitlements) ListByMember(_ context.Context, memberID string) ([]store.Entitlement, error) {
	return f.byMember[memberID], f.memberErr
}

func (f fakeMeEntitlements) ListByTeam(_ context.Context, teamID string) ([]store.Entitlement, error) {
	return f.byTeam[teamID], f.teamErr
}

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

// setMeEntitlementStore points the newMeEntitlementStore seam at s for the test,
// restoring the production factory afterwards. It mutates a package-level var,
// so callers must not run in parallel.
func setMeEntitlementStore(t *testing.T, s meEntitlementReader) {
	t.Helper()
	orig := newMeEntitlementStore
	newMeEntitlementStore = func(store.Querier) meEntitlementReader { return s }
	t.Cleanup(func() { newMeEntitlementStore = orig })
}

// setTeamMembershipStore points the newTeamMembershipStore seam at s for the
// test, restoring the production factory afterwards.
func setTeamMembershipStore(t *testing.T, s teamMembershipReader) {
	t.Helper()
	orig := newTeamMembershipStore
	newTeamMembershipStore = func(store.Querier) teamMembershipReader { return s }
	t.Cleanup(func() { newTeamMembershipStore = orig })
}

// meHarness wires a Deps with a shared issuer/verifier key, points the three
// store seams /me/agents reads through at the supplied fakes, mounts the
// router, and returns it with an Authorization header for member-1 in orgID
// (agentToken mints member_id "member-1").
func meHarness(t *testing.T, ents meEntitlementReader, teams teamMembershipReader, agents agentStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, nil, fakeMembers{})
	setMeEntitlementStore(t, ents)
	setTeamMembershipStore(t, teams)
	setAgentStore(t, agents)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

// TestListMyAgentsUnion proves /me/agents returns the union of the caller's
// direct member grants and their teams' grants, deduplicated and org-scoped: a
// duplicate agent granted both directly and via a team appears once, and an
// agent reachable through a grant but living in another org is dropped.
func TestListMyAgentsUnion(t *testing.T) {
	created := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	ents := fakeMeEntitlements{
		byMember: map[string][]store.Entitlement{
			"member-1": {{AgentID: "a1", MemberID: strptr("member-1")}},
		},
		byTeam: map[string][]store.Entitlement{
			"t1": {
				{AgentID: "a2", TeamID: strptr("t1")},
				{AgentID: "a1", TeamID: strptr("t1")}, // duplicate of the direct grant
			},
			"t2": {{AgentID: "a3", TeamID: strptr("t2")}}, // a3 lives in another org
		},
	}
	teams := &fakeTeamMembershipStore{teams: []store.Team{{ID: "t1"}, {ID: "t2"}}}
	agents := mapAgentStore{agents: map[string]store.Agent{
		"a1": storeAgent("a1", "org-1", "one", nil, 1, created),
		"a2": storeAgent("a2", "org-1", "two", strptr("second"), 2, created),
		"a3": storeAgent("a3", "org-2", "cross-org", nil, 1, created),
	}}

	h, hdr := meHarness(t, ents, teams, agents, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// The teams read must be scoped to the caller.
	if teams.gotMemberID != "member-1" {
		t.Errorf("ListTeams member = %q, want member-1", teams.gotMemberID)
	}

	var body apiAgentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// a1 (direct, first) then a2 (team); a1's team duplicate is deduped; a3 is
	// dropped as cross-org.
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
// caller has no entitlements and no teams.
func TestListMyAgentsEmpty(t *testing.T) {
	ents := fakeMeEntitlements{}
	teams := &fakeTeamMembershipStore{}
	agents := mapAgentStore{agents: map[string]store.Agent{}}

	h, hdr := meHarness(t, ents, teams, agents, "org-1")
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
	h, _ := meHarness(t, fakeMeEntitlements{}, &fakeTeamMembershipStore{}, mapAgentStore{}, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route mounted, missing token)", rec.Code)
	}
}

// TestListMyAgentsEntitlementError proves a store failure resolving the caller's
// grants surfaces as a 500 rather than a partial list.
func TestListMyAgentsEntitlementError(t *testing.T) {
	ents := fakeMeEntitlements{memberErr: errors.New("boom")}
	h, hdr := meHarness(t, ents, &fakeTeamMembershipStore{}, mapAgentStore{}, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/me/agents", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestListMyAgentsSkipsDeletedGrant proves a grant pointing at an agent that no
// longer exists (GetByID → not found) is skipped rather than failing the list.
func TestListMyAgentsSkipsDeletedGrant(t *testing.T) {
	created := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	ents := fakeMeEntitlements{
		byMember: map[string][]store.Entitlement{
			"member-1": {
				{AgentID: "gone", MemberID: strptr("member-1")},
				{AgentID: "a1", MemberID: strptr("member-1")},
			},
		},
	}
	agents := mapAgentStore{agents: map[string]store.Agent{
		"a1": storeAgent("a1", "org-1", "one", nil, 1, created),
	}}

	h, hdr := meHarness(t, ents, &fakeTeamMembershipStore{}, agents, "org-1")
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
