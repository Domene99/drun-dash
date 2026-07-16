package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/store"
)

// fakeAgentStore is an in-memory agentStore: it returns canned rows/errors and
// records the arguments each method was called with, so the handler tests can
// assert on org scoping and the pagination lookahead without a database.
type fakeAgentStore struct {
	createAgent store.Agent
	createErr   error
	getAgent    store.Agent
	getErr      error
	listAgents  []store.Agent
	listErr     error
	updateAgent store.Agent
	updateErr   error
	deleteErr   error

	setVersionAgent  store.Agent
	setVersionErr    error
	setVersionCalled bool

	// Captured call arguments.
	gotCreateOrg    string
	gotCreateName   string
	gotCreateDesc   *string
	gotGetID        string
	gotListOrg      string
	gotListLimit    int
	gotListAfterC   time.Time
	gotListAfterI   string
	gotUpdateID     string
	gotUpdateName   string
	gotUpdateDesc   *string
	gotDeleteID     string
	gotSetVersionID string
	gotSetVersion   int
}

func (f *fakeAgentStore) Create(_ context.Context, orgID, name string, description *string) (store.Agent, error) {
	f.gotCreateOrg, f.gotCreateName, f.gotCreateDesc = orgID, name, description
	return f.createAgent, f.createErr
}

func (f *fakeAgentStore) GetByID(_ context.Context, id string) (store.Agent, error) {
	f.gotGetID = id
	return f.getAgent, f.getErr
}

func (f *fakeAgentStore) ListByOrgPage(_ context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.Agent, error) {
	f.gotListOrg, f.gotListLimit, f.gotListAfterC, f.gotListAfterI = orgID, limit, afterCreatedAt, afterID
	return f.listAgents, f.listErr
}

func (f *fakeAgentStore) Update(_ context.Context, id, name string, description *string) (store.Agent, error) {
	f.gotUpdateID, f.gotUpdateName, f.gotUpdateDesc = id, name, description
	return f.updateAgent, f.updateErr
}

func (f *fakeAgentStore) Delete(_ context.Context, id string) error {
	f.gotDeleteID = id
	return f.deleteErr
}

func (f *fakeAgentStore) SetCurrentVersion(_ context.Context, id string, version int) (store.Agent, error) {
	f.gotSetVersionID, f.gotSetVersion, f.setVersionCalled = id, version, true
	return f.setVersionAgent, f.setVersionErr
}

// setAgentStore points the newAgentStore seam at s for the duration of the
// test, restoring the production factory afterwards. Because it mutates a
// package-level var, callers must not run in parallel.
func setAgentStore(t *testing.T, s agentStore) {
	t.Helper()
	orig := newAgentStore
	newAgentStore = func(store.Querier) agentStore { return s }
	t.Cleanup(func() { newAgentStore = orig })
}

// agentToken mints an access token for orgID so protected agent routes accept
// the request. The subject/roles are fixed; only the org matters to these tests.
func agentToken(t *testing.T, d Deps, orgID string) string {
	t.Helper()
	tok, err := d.Issuer.Mint(context.Background(), auth.Claims{
		OrgID:            orgID,
		MemberID:         "member-1",
		Roles:            []string{"admin"},
		RegisteredClaims: jwt.RegisteredClaims{Subject: "idp-sub-1"},
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

// agentHarness wires a Deps (with a shared issuer/verifier key), points the
// store seam at fake, mounts the router, and returns a ready Authorization
// header for orgID.
func agentHarness(t *testing.T, fake agentStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAgentStore(t, fake)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

func storeAgent(id, org, name string, desc *string, version int, created time.Time) store.Agent {
	return store.Agent{
		ID:             id,
		OrgID:          org,
		Name:           name,
		Description:    desc,
		CurrentVersion: version,
		CreatedAt:      created,
		UpdatedAt:      created,
	}
}

func strptr(s string) *string { return &s }

// TestListAgentsPage proves the keyset lookahead: the handler asks the store
// for limit+1 rows, drops the extra, returns exactly limit items, and sets
// next_cursor from the last kept row.
func TestListAgentsPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{listAgents: []store.Agent{
		storeAgent("a1", "org-1", "one", nil, 0, t1),
		storeAgent("a2", "org-1", "two", strptr("second"), 3, t2),
		storeAgent("a3", "org-1", "three", nil, 0, t3), // the lookahead row
	}}
	h, hdr := agentHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/agents?limit=2", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// The store must be asked for limit+1, scoped to the caller's org.
	if fake.gotListOrg != "org-1" {
		t.Errorf("list org = %q, want org-1", fake.gotListOrg)
	}
	if fake.gotListLimit != 3 {
		t.Errorf("list limit = %d, want 3 (limit+1 lookahead)", fake.gotListLimit)
	}

	var body apiAgentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[1].LatestVersion != 3 || body.Items[1].Description == nil || *body.Items[1].Description != "second" {
		t.Errorf("second item mapped wrong: %+v", body.Items[1])
	}
	if body.NextCursor == nil {
		t.Fatal("next_cursor = nil, want a cursor for the last kept row")
	}
	cur, err := DecodeCursor(*body.NextCursor)
	if err != nil {
		t.Fatalf("decode next_cursor: %v", err)
	}
	if cur.ID != "a2" || !cur.CreatedAt.Equal(t2) {
		t.Errorf("next_cursor = (%v, %s), want (%v, a2)", cur.CreatedAt, cur.ID, t2)
	}
}

// TestListAgentsLastPage proves that a short page (≤ limit rows) reports a null
// next_cursor and that an incoming cursor is decoded and forwarded to the store.
func TestListAgentsLastPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{listAgents: []store.Agent{
		storeAgent("a1", "org-1", "one", nil, 0, t1),
	}}
	h, hdr := agentHarness(t, fake, "org-1")

	cursor := EncodeCursor(Cursor{CreatedAt: t1, ID: "a0"})
	rec := doJSON(t, h, http.MethodGet, "/agents?limit=5&cursor="+cursor, "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotListAfterI != "a0" || !fake.gotListAfterC.Equal(t1) {
		t.Errorf("seek = (%v, %s), want (%v, a0)", fake.gotListAfterC, fake.gotListAfterI, t1)
	}

	var body apiAgentList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.NextCursor != nil {
		t.Errorf("next_cursor = %q, want null", *body.NextCursor)
	}
	if len(body.Items) != 1 {
		t.Errorf("items = %d, want 1", len(body.Items))
	}
}

// TestListAgentsEmpty proves items serialises as [] (never null) when the org
// has no agents.
func TestListAgentsEmpty(t *testing.T) {
	fake := &fakeAgentStore{listAgents: nil}
	h, hdr := agentHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/agents", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got == "" || !containsItemsArray(got) {
		t.Errorf("body = %s, want items:[] present", got)
	}
	// Default limit is 50, so the lookahead asks for 51.
	if fake.gotListLimit != 51 {
		t.Errorf("list limit = %d, want 51 (default 50 + lookahead)", fake.gotListLimit)
	}
}

func containsItemsArray(body string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	items, ok := m["items"]
	return ok && string(items) == "[]"
}

func TestListAgentsBadParams(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"non-integer limit", "/agents?limit=abc"},
		{"malformed cursor", "/agents?cursor=notacursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentStore{}
			h, hdr := agentHarness(t, fake, "org-1")
			rec := doJSON(t, h, http.MethodGet, tc.path, "", hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestListAgentsStoreError(t *testing.T) {
	fake := &fakeAgentStore{listErr: context.DeadlineExceeded}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/agents", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateAgent proves the happy path maps store → 201 Agent and that the org
// comes from the token (not the body), name/description are forwarded, and a
// supplied default_policy_id is accepted and ignored.
func TestCreateAgent(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{
		createAgent: storeAgent("a9", "org-1", "planner", strptr("plans things"), 0, created),
	}
	h, hdr := agentHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/agents",
		`{"name":"planner","description":"plans things","default_policy_id":"pol-1"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotCreateOrg != "org-1" {
		t.Errorf("create org = %q, want org-1 (from token)", fake.gotCreateOrg)
	}
	if fake.gotCreateName != "planner" {
		t.Errorf("create name = %q, want planner", fake.gotCreateName)
	}
	if fake.gotCreateDesc == nil || *fake.gotCreateDesc != "plans things" {
		t.Errorf("create desc = %v, want plans things", fake.gotCreateDesc)
	}

	var body apiAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != "a9" || body.Name != "planner" {
		t.Errorf("body = %+v, want id a9 name planner", body)
	}
	// default_policy_id has no backing column and must never be emitted.
	if _, ok := rawKeys(t, rec.Body.Bytes())["default_policy_id"]; ok {
		t.Error("response contains default_policy_id; it must be omitted")
	}
}

func TestCreateAgentValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing name", `{"description":"x"}`, http.StatusBadRequest},
		{"empty name", `{"name":""}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
		{"unknown field", `{"name":"x","bogus":1}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentStore{}
			h, hdr := agentHarness(t, fake, "org-1")
			rec := doJSON(t, h, http.MethodPost, "/agents", tc.body, hdr)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.code, rec.Body.String())
			}
		})
	}
}

func TestCreateAgentConflict(t *testing.T) {
	fake := &fakeAgentStore{createErr: store.ErrConflict}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodPost, "/agents", `{"name":"dup"}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestGetAgent(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{getAgent: storeAgent("a5", "org-1", "reader", nil, 2, created)}
	h, hdr := agentHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/agents/a5", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotGetID != "a5" {
		t.Errorf("get id = %q, want a5", fake.gotGetID)
	}
	var body apiAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.LatestVersion != 2 {
		t.Errorf("latest_version = %d, want 2", body.LatestVersion)
	}
	// A nil description is omitted from the body entirely.
	if _, ok := rawKeys(t, rec.Body.Bytes())["description"]; ok {
		t.Error("response contains description; it must be omitted when nil")
	}
}

// TestGetAgentCrossOrg proves a row owned by another org is reported as 404,
// not 403, so a caller cannot probe for another tenant's ids (ADR-0002 §1).
func TestGetAgentCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{getAgent: storeAgent("a5", "org-2", "other", nil, 0, created)}
	h, hdr := agentHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/agents/a5", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestGetAgentNotFound(t *testing.T) {
	fake := &fakeAgentStore{getErr: store.ErrNotFound}
	h, hdr := agentHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/agents/missing", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAgentRoutesRequireAuth proves every agent route sits behind RequireAuth:
// a request with no token is a 401 and never reaches the store.
func TestAgentRoutesRequireAuth(t *testing.T) {
	tests := []struct {
		method, path string
	}{
		{http.MethodGet, "/agents"},
		{http.MethodPost, "/agents"},
		{http.MethodGet, "/agents/a1"},
		{http.MethodPatch, "/agents/a1"},
		{http.MethodDelete, "/agents/a1"},
		{http.MethodGet, "/agents/a1/versions"},
		{http.MethodPost, "/agents/a1/versions"},
		{http.MethodGet, "/agents/a1/entitlements"},
		{http.MethodPut, "/agents/a1/entitlements"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setAgentStore(t, &fakeAgentStore{})
			h := mountRouter(deps)
			rec := doJSON(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func rawKeys(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	return m
}
