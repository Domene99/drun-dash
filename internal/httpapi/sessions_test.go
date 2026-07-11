package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakeSessionStore is an in-memory sessionStore: it returns canned rows/errors
// and records the arguments each method was called with, so the handler tests
// can assert on org scoping and the pagination lookahead without a database.
type fakeSessionStore struct {
	getSession   store.Session
	getErr       error
	listSessions []store.Session
	listErr      error

	// Captured call arguments.
	gotGetID      string
	gotListOrg    string
	gotListLimit  int
	gotListAfterC time.Time
	gotListAfterI string
}

func (f *fakeSessionStore) GetByID(_ context.Context, id string) (store.Session, error) {
	f.gotGetID = id
	return f.getSession, f.getErr
}

func (f *fakeSessionStore) ListByOrgPage(_ context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.Session, error) {
	f.gotListOrg, f.gotListLimit, f.gotListAfterC, f.gotListAfterI = orgID, limit, afterCreatedAt, afterID
	return f.listSessions, f.listErr
}

// setSessionStore points the newSessionStore seam at s for the duration of the
// test, restoring the production factory afterwards. Because it mutates a
// package-level var, callers must not run in parallel.
func setSessionStore(t *testing.T, s sessionStore) {
	t.Helper()
	orig := newSessionStore
	newSessionStore = func(store.Querier) sessionStore { return s }
	t.Cleanup(func() { newSessionStore = orig })
}

func sessionHarness(t *testing.T, fake sessionStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setSessionStore(t, fake)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

func storeSession(id, org, agentID string, version *int, status store.SessionStatus, fanOut int, created time.Time, terminated *time.Time) store.Session {
	return store.Session{
		ID:            id,
		OrgID:         org,
		AgentID:       agentID,
		AgentVersion:  version,
		Status:        status,
		FanOutCeiling: fanOut,
		CreatedAt:     created,
		TerminatedAt:  terminated,
	}
}

func intptr(i int) *int { return &i }

// TestListSessionsPage proves the keyset lookahead: the handler asks the store
// for limit+1 rows, drops the extra, returns exactly limit items, and sets
// next_cursor from the last kept row.
func TestListSessionsPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fake := &fakeSessionStore{listSessions: []store.Session{
		storeSession("s1", "org-1", "agt_1", nil, store.SessionStatusPending, 1, t1, nil),
		storeSession("s2", "org-1", "agt_2", intptr(3), store.SessionStatusActive, 4, t2, nil),
		storeSession("s3", "org-1", "agt_3", nil, store.SessionStatusPending, 1, t3, nil), // lookahead row
	}}
	h, hdr := sessionHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions?limit=2", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotListOrg != "org-1" {
		t.Errorf("list org = %q, want org-1", fake.gotListOrg)
	}
	if fake.gotListLimit != 3 {
		t.Errorf("list limit = %d, want 3 (limit+1 lookahead)", fake.gotListLimit)
	}

	var body apiSessionList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[1].SessionID != "s2" || body.Items[1].AgentID != "agt_2" ||
		body.Items[1].Status != "active" || body.Items[1].FanOutCeiling != 4 ||
		body.Items[1].AgentVersion == nil || *body.Items[1].AgentVersion != 3 {
		t.Errorf("second item mapped wrong: %+v", body.Items[1])
	}
	if body.NextCursor == nil {
		t.Fatal("next_cursor = nil, want a cursor for the last kept row")
	}
	cur, err := DecodeCursor(*body.NextCursor)
	if err != nil {
		t.Fatalf("decode next_cursor: %v", err)
	}
	if cur.ID != "s2" || !cur.CreatedAt.Equal(t2) {
		t.Errorf("next_cursor = (%v, %s), want (%v, s2)", cur.CreatedAt, cur.ID, t2)
	}
}

// TestListSessionsLastPage proves a short page reports a null next_cursor and
// that an incoming cursor is decoded and forwarded to the store.
func TestListSessionsLastPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	fake := &fakeSessionStore{listSessions: []store.Session{
		storeSession("s1", "org-1", "agt_1", nil, store.SessionStatusPending, 1, t1, nil),
	}}
	h, hdr := sessionHarness(t, fake, "org-1")

	cursor := EncodeCursor(Cursor{CreatedAt: t1, ID: "s0"})
	rec := doJSON(t, h, http.MethodGet, "/sessions?limit=5&cursor="+cursor, "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotListAfterI != "s0" || !fake.gotListAfterC.Equal(t1) {
		t.Errorf("seek = (%v, %s), want (%v, s0)", fake.gotListAfterC, fake.gotListAfterI, t1)
	}

	var body apiSessionList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.NextCursor != nil {
		t.Errorf("next_cursor = %q, want null", *body.NextCursor)
	}
	// agent_version and terminated_at are omitted when nil.
	itemKeys := sessionItemKeys(t, rec.Body.Bytes())
	if _, ok := itemKeys["agent_version"]; ok {
		t.Error("agent_version present; must be omitted when nil")
	}
	if _, ok := itemKeys["terminated_at"]; ok {
		t.Error("terminated_at present; must be omitted when nil")
	}
}

// TestListSessionsEmpty proves items serialises as [] (never null) when the org
// has no sessions, and that the default limit drives a 51-row lookahead.
func TestListSessionsEmpty(t *testing.T) {
	fake := &fakeSessionStore{listSessions: nil}
	h, hdr := sessionHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if items := rawKeys(t, rec.Body.Bytes())["items"]; string(items) != "[]" {
		t.Errorf("items = %s, want []", items)
	}
	if fake.gotListLimit != 51 {
		t.Errorf("list limit = %d, want 51 (default 50 + lookahead)", fake.gotListLimit)
	}
}

func TestListSessionsBadParams(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"non-integer limit", "/sessions?limit=abc"},
		{"malformed cursor", "/sessions?cursor=notacursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSessionStore{}
			h, hdr := sessionHarness(t, fake, "org-1")
			rec := doJSON(t, h, http.MethodGet, tc.path, "", hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestListSessionsStoreError(t *testing.T) {
	fake := &fakeSessionStore{listErr: context.DeadlineExceeded}
	h, hdr := sessionHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/sessions", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestGetSession proves the happy path maps store → 200 Session, forwards the
// id, and emits terminated_at when set.
func TestGetSession(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	term := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeSessionStore{getSession: storeSession(
		"s5", "org-1", "agt_9", intptr(2), store.SessionStatusTerminated, 3, created, &term)}
	h, hdr := sessionHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions/s5", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotGetID != "s5" {
		t.Errorf("get id = %q, want s5", fake.gotGetID)
	}
	var body apiSession
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SessionID != "s5" || body.AgentID != "agt_9" || body.Status != "terminated" {
		t.Errorf("body = %+v, want id s5 agent agt_9 terminated", body)
	}
	if body.TerminatedAt == nil || !body.TerminatedAt.Equal(term) {
		t.Errorf("terminated_at = %v, want %v", body.TerminatedAt, term)
	}
	// policy_id / policy_version are omitted in v1.
	keys := rawKeys(t, rec.Body.Bytes())
	if _, ok := keys["policy_id"]; ok {
		t.Error("policy_id present; must be omitted in v1")
	}
	if _, ok := keys["policy_version"]; ok {
		t.Error("policy_version present; must be omitted in v1")
	}
}

// TestGetSessionCrossOrg proves a session owned by another org is reported as
// 404, not 403, so a caller cannot probe another tenant's ids (ADR-0002 §1).
func TestGetSessionCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	fake := &fakeSessionStore{getSession: storeSession(
		"s5", "org-2", "agt_9", nil, store.SessionStatusActive, 1, created, nil)}
	h, hdr := sessionHarness(t, fake, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/sessions/s5", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestGetSessionNotFound(t *testing.T) {
	fake := &fakeSessionStore{getErr: store.ErrNotFound}
	h, hdr := sessionHarness(t, fake, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/sessions/missing", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestSessionRoutesRequireAuth proves every session route sits behind
// RequireAuth: a request with no token is a 401 and never reaches the store.
func TestSessionRoutesRequireAuth(t *testing.T) {
	tests := []struct {
		method, path string
	}{
		{http.MethodGet, "/sessions"},
		{http.MethodGet, "/sessions/s1"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setSessionStore(t, &fakeSessionStore{})
			h := mountRouter(deps)
			rec := doJSON(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// sessionItemKeys returns the JSON object keys of the first item in a
// SessionList body, for asserting omitted fields.
func sessionItemKeys(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(envelope.Items) == 0 {
		t.Fatal("no items to inspect")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Items[0], &m); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	return m
}
