package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakeApprovalStore is an in-memory approvalStore: it returns canned
// rows/errors and records the arguments each method was called with, so the
// handler tests can assert on org scoping, the pagination lookahead, and the
// decision mapping without a database.
type fakeApprovalStore struct {
	createApproval store.ApprovalRequest
	createErr      error
	getApproval    store.ApprovalRequest
	getErr         error
	listApprovals  []store.ApprovalRequest
	listErr        error
	decideApproval store.ApprovalRequest
	decideErr      error

	// Captured call arguments.
	gotCreate       store.CreateApprovalRequestParams
	gotGetID        string
	gotListOrg      string
	gotListLimit    int
	gotListAfterC   time.Time
	gotListAfterI   string
	gotDecideID     string
	gotDecideStatus store.ApprovalStatus
	gotDecideBy     *string
}

func (f *fakeApprovalStore) Create(_ context.Context, p store.CreateApprovalRequestParams) (store.ApprovalRequest, error) {
	f.gotCreate = p
	return f.createApproval, f.createErr
}

func (f *fakeApprovalStore) GetByID(_ context.Context, id string) (store.ApprovalRequest, error) {
	f.gotGetID = id
	return f.getApproval, f.getErr
}

func (f *fakeApprovalStore) ListByOrgPage(_ context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.ApprovalRequest, error) {
	f.gotListOrg, f.gotListLimit, f.gotListAfterC, f.gotListAfterI = orgID, limit, afterCreatedAt, afterID
	return f.listApprovals, f.listErr
}

func (f *fakeApprovalStore) Decide(_ context.Context, id string, status store.ApprovalStatus, decidedBy *string) (store.ApprovalRequest, error) {
	f.gotDecideID, f.gotDecideStatus, f.gotDecideBy = id, status, decidedBy
	return f.decideApproval, f.decideErr
}

// setApprovalStore points the newApprovalStore seam at s for the duration of
// the test, restoring the production factory afterwards. Because it mutates a
// package-level var, callers must not run in parallel.
func setApprovalStore(t *testing.T, s approvalStore) {
	t.Helper()
	orig := newApprovalStore
	newApprovalStore = func(store.Querier) approvalStore { return s }
	t.Cleanup(func() { newApprovalStore = orig })
}

// approvalHarness wires a Deps, points the approval store seam (and the session
// store seam that createApproval validates against) at the fakes, mounts the
// router, and returns a ready Authorization header for orgID.
func approvalHarness(t *testing.T, fake approvalStore, sess sessionStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setApprovalStore(t, fake)
	if sess == nil {
		sess = &fakeSessionStore{}
	}
	setSessionStore(t, sess)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

func storeApproval(id, org, sessionID, action string, reason *string, status store.ApprovalStatus, created time.Time) store.ApprovalRequest {
	return store.ApprovalRequest{
		ID:          id,
		OrgID:       org,
		SessionID:   sessionID,
		Action:      action,
		Reason:      reason,
		ContextJSON: json.RawMessage("{}"),
		Status:      status,
		CreatedAt:   created,
	}
}

// TestListApprovalsPage proves the keyset lookahead: the handler asks the store
// for limit+1 rows, drops the extra, returns exactly limit items, and sets
// next_cursor from the last kept row.
func TestListApprovalsPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	fake := &fakeApprovalStore{listApprovals: []store.ApprovalRequest{
		storeApproval("ap1", "org-1", "s1", "spend", nil, store.ApprovalStatusPending, t1),
		storeApproval("ap2", "org-1", "s2", "delete", strptr("over budget"), store.ApprovalStatusApproved, t2),
		storeApproval("ap3", "org-1", "s3", "email", nil, store.ApprovalStatusPending, t3), // lookahead row
	}}
	h, hdr := approvalHarness(t, fake, nil, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/approvals?limit=2", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotListOrg != "org-1" {
		t.Errorf("list org = %q, want org-1", fake.gotListOrg)
	}
	if fake.gotListLimit != 3 {
		t.Errorf("list limit = %d, want 3 (limit+1 lookahead)", fake.gotListLimit)
	}

	var body apiApprovalList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[1].Status != "approved" || body.Items[1].Reason == nil || *body.Items[1].Reason != "over budget" {
		t.Errorf("second item mapped wrong: %+v", body.Items[1])
	}
	if body.NextCursor == nil {
		t.Fatal("next_cursor = nil, want a cursor for the last kept row")
	}
	cur, err := DecodeCursor(*body.NextCursor)
	if err != nil {
		t.Fatalf("decode next_cursor: %v", err)
	}
	if cur.ID != "ap2" || !cur.CreatedAt.Equal(t2) {
		t.Errorf("next_cursor = (%v, %s), want (%v, ap2)", cur.CreatedAt, cur.ID, t2)
	}
}

// TestListApprovalsLastPage proves a short page reports a null next_cursor and
// that an incoming cursor is decoded and forwarded to the store.
func TestListApprovalsLastPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	fake := &fakeApprovalStore{listApprovals: []store.ApprovalRequest{
		storeApproval("ap1", "org-1", "s1", "spend", nil, store.ApprovalStatusPending, t1),
	}}
	h, hdr := approvalHarness(t, fake, nil, "org-1")

	cursor := EncodeCursor(Cursor{CreatedAt: t1, ID: "ap0"})
	rec := doJSON(t, h, http.MethodGet, "/approvals?limit=5&cursor="+cursor, "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotListAfterI != "ap0" || !fake.gotListAfterC.Equal(t1) {
		t.Errorf("seek = (%v, %s), want (%v, ap0)", fake.gotListAfterC, fake.gotListAfterI, t1)
	}

	var body apiApprovalList
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

// TestListApprovalsEmpty proves items serialises as [] (never null) when the
// org has no approval requests.
func TestListApprovalsEmpty(t *testing.T) {
	fake := &fakeApprovalStore{listApprovals: nil}
	h, hdr := approvalHarness(t, fake, nil, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/approvals", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got == "" || !containsItemsArray(got) {
		t.Errorf("body = %s, want items:[] present", got)
	}
}

func TestListApprovalsBadParams(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"non-integer limit", "/approvals?limit=abc"},
		{"malformed cursor", "/approvals?cursor=notacursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeApprovalStore{}
			h, hdr := approvalHarness(t, fake, nil, "org-1")
			rec := doJSON(t, h, http.MethodGet, tc.path, "", hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestListApprovalsStoreError(t *testing.T) {
	fake := &fakeApprovalStore{listErr: context.DeadlineExceeded}
	h, hdr := approvalHarness(t, fake, nil, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/approvals", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateApproval proves the happy path maps store → 201 Approval, that the
// org comes from the token (not the body), the pending session is validated,
// and session_id/action/reason/context are forwarded to the store.
func TestCreateApproval(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeApprovalStore{
		createApproval: storeApproval("ap9", "org-1", "s1", "spend", strptr("over budget"), store.ApprovalStatusPending, created),
	}
	sess := &fakeSessionStore{getSession: storeSession("s1", "org-1", "a1", nil, store.SessionStatusActive, 1, created, nil)}
	h, hdr := approvalHarness(t, fake, sess, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/approvals",
		`{"session_id":"s1","action":"spend","reason":"over budget","context":{"amount":42}}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if sess.gotGetID != "s1" {
		t.Errorf("validated session = %q, want s1", sess.gotGetID)
	}
	if fake.gotCreate.OrgID != "org-1" {
		t.Errorf("create org = %q, want org-1 (from token)", fake.gotCreate.OrgID)
	}
	if fake.gotCreate.SessionID != "s1" || fake.gotCreate.Action != "spend" {
		t.Errorf("create = %+v, want session s1 action spend", fake.gotCreate)
	}
	if fake.gotCreate.Reason == nil || *fake.gotCreate.Reason != "over budget" {
		t.Errorf("create reason = %v, want over budget", fake.gotCreate.Reason)
	}
	if string(fake.gotCreate.ContextJSON) != `{"amount":42}` {
		t.Errorf("create context = %s, want {\"amount\":42}", fake.gotCreate.ContextJSON)
	}

	var body apiApproval
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != "ap9" || body.Status != "pending" {
		t.Errorf("body = %+v, want id ap9 status pending", body)
	}
	// A pending request has no decision: decided_by/decided_at are omitted.
	keys := rawKeys(t, rec.Body.Bytes())
	if _, ok := keys["decided_by"]; ok {
		t.Error("response contains decided_by; it must be omitted while pending")
	}
	if _, ok := keys["decided_at"]; ok {
		t.Error("response contains decided_at; it must be omitted while pending")
	}
}

// TestCreateApprovalCrossOrgSession proves a session owned by another org is a
// 404 (not 403) and no approval is created.
func TestCreateApprovalCrossOrgSession(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeApprovalStore{}
	sess := &fakeSessionStore{getSession: storeSession("s1", "org-2", "a1", nil, store.SessionStatusActive, 1, created, nil)}
	h, hdr := approvalHarness(t, fake, sess, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/approvals", `{"session_id":"s1","action":"spend"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotCreate.SessionID != "" {
		t.Error("approval was created for a cross-org session")
	}
}

// TestCreateApprovalMissingSession proves a missing session surfaces as 404.
func TestCreateApprovalMissingSession(t *testing.T) {
	fake := &fakeApprovalStore{}
	sess := &fakeSessionStore{getErr: store.ErrNotFound}
	h, hdr := approvalHarness(t, fake, sess, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/approvals", `{"session_id":"ghost","action":"spend"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCreateApprovalValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing session_id", `{"action":"spend"}`},
		{"empty session_id", `{"session_id":"","action":"spend"}`},
		{"missing action", `{"session_id":"s1"}`},
		{"empty action", `{"session_id":"s1","action":""}`},
		{"malformed json", `{`},
		{"unknown field", `{"session_id":"s1","action":"spend","bogus":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeApprovalStore{}
			h, hdr := approvalHarness(t, fake, nil, "org-1")
			rec := doJSON(t, h, http.MethodPost, "/approvals", tc.body, hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDecideApproval proves approve→approved and deny→denied map correctly, the
// principal's member id is recorded as decided_by, and the updated row is
// returned.
func TestDecideApproval(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	decidedAt := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name       string
		decision   string
		wantStatus store.ApprovalStatus
	}{
		{"approve", `{"decision":"approve"}`, store.ApprovalStatusApproved},
		{"deny with reason", `{"decision":"deny","reason":"not justified"}`, store.ApprovalStatusDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decided := storeApproval("ap1", "org-1", "s1", "spend", nil, tc.wantStatus, created)
			decided.DecidedBy = strptr("member-1")
			decided.DecidedAt = &decidedAt
			fake := &fakeApprovalStore{
				getApproval:    storeApproval("ap1", "org-1", "s1", "spend", nil, store.ApprovalStatusPending, created),
				decideApproval: decided,
			}
			h, hdr := approvalHarness(t, fake, nil, "org-1")

			rec := doJSON(t, h, http.MethodPost, "/approvals/ap1/decision", tc.decision, hdr)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			if fake.gotDecideID != "ap1" {
				t.Errorf("decide id = %q, want ap1", fake.gotDecideID)
			}
			if fake.gotDecideStatus != tc.wantStatus {
				t.Errorf("decide status = %q, want %q", fake.gotDecideStatus, tc.wantStatus)
			}
			if fake.gotDecideBy == nil || *fake.gotDecideBy != "member-1" {
				t.Errorf("decided_by = %v, want member-1", fake.gotDecideBy)
			}

			var body apiApproval
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Status != string(tc.wantStatus) {
				t.Errorf("status = %q, want %q", body.Status, tc.wantStatus)
			}
			if body.DecidedBy == nil || *body.DecidedBy != "member-1" {
				t.Errorf("decided_by = %v, want member-1", body.DecidedBy)
			}
		})
	}
}

// TestDecideApprovalAlreadyDecided proves a request that is no longer pending is
// a 409 conflict and Decide is never called.
func TestDecideApprovalAlreadyDecided(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeApprovalStore{
		getApproval: storeApproval("ap1", "org-1", "s1", "spend", nil, store.ApprovalStatusApproved, created),
	}
	h, hdr := approvalHarness(t, fake, nil, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/approvals/ap1/decision", `{"decision":"deny"}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotDecideID != "" {
		t.Error("Decide was called on an already-decided request")
	}
}

// TestDecideApprovalCrossOrg proves a request owned by another org is a 404
// (not 403) and Decide is never called.
func TestDecideApprovalCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeApprovalStore{
		getApproval: storeApproval("ap1", "org-2", "s1", "spend", nil, store.ApprovalStatusPending, created),
	}
	h, hdr := approvalHarness(t, fake, nil, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/approvals/ap1/decision", `{"decision":"approve"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotDecideID != "" {
		t.Error("Decide was called on a cross-org request")
	}
}

func TestDecideApprovalNotFound(t *testing.T) {
	fake := &fakeApprovalStore{getErr: store.ErrNotFound}
	h, hdr := approvalHarness(t, fake, nil, "org-1")
	rec := doJSON(t, h, http.MethodPost, "/approvals/missing/decision", `{"decision":"approve"}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestDecideApprovalBadDecision(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing decision", `{}`},
		{"unknown decision", `{"decision":"maybe"}`},
		{"malformed json", `{`},
		{"unknown field", `{"decision":"approve","bogus":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeApprovalStore{
				getApproval: storeApproval("ap1", "org-1", "s1", "spend", nil, store.ApprovalStatusPending, time.Now().UTC()),
			}
			h, hdr := approvalHarness(t, fake, nil, "org-1")
			rec := doJSON(t, h, http.MethodPost, "/approvals/ap1/decision", tc.body, hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestApprovalRoutesRequireAuth proves every approval route sits behind
// RequireAuth: a request with no token is a 401 and never reaches the store.
func TestApprovalRoutesRequireAuth(t *testing.T) {
	tests := []struct {
		method, path string
	}{
		{http.MethodGet, "/approvals"},
		{http.MethodPost, "/approvals"},
		{http.MethodPost, "/approvals/ap1/decision"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setApprovalStore(t, &fakeApprovalStore{})
			setSessionStore(t, &fakeSessionStore{})
			h := mountRouter(deps)
			rec := doJSON(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}
