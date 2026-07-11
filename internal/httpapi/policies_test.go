package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakePolicyStore is an in-memory policyStore: it returns canned rows/errors and
// records the arguments each method was called with, so the handler tests can
// assert on org scoping, the pagination lookahead, and the current-version
// bump without a database.
type fakePolicyStore struct {
	createPolicy store.Policy
	createErr    error
	getPolicy    store.Policy
	getErr       error
	listPolicies []store.Policy
	listErr      error
	setPolicy    store.Policy
	setErr       error

	// Captured call arguments.
	gotCreateOrg  string
	gotCreateName string
	gotCreateDesc *string
	gotGetID      string
	gotListOrg    string
	gotListLimit  int
	gotListAfterC time.Time
	gotListAfterI string
	gotSetID      string
	gotSetVersion int
}

func (f *fakePolicyStore) Create(_ context.Context, orgID, name string, description *string) (store.Policy, error) {
	f.gotCreateOrg, f.gotCreateName, f.gotCreateDesc = orgID, name, description
	return f.createPolicy, f.createErr
}

func (f *fakePolicyStore) GetByID(_ context.Context, id string) (store.Policy, error) {
	f.gotGetID = id
	return f.getPolicy, f.getErr
}

func (f *fakePolicyStore) ListByOrgPage(_ context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.Policy, error) {
	f.gotListOrg, f.gotListLimit, f.gotListAfterC, f.gotListAfterI = orgID, limit, afterCreatedAt, afterID
	return f.listPolicies, f.listErr
}

func (f *fakePolicyStore) SetCurrentVersion(_ context.Context, id string, version int) (store.Policy, error) {
	f.gotSetID, f.gotSetVersion = id, version
	if f.setErr != nil {
		return store.Policy{}, f.setErr
	}
	// Default to echoing the created policy with its version advanced, so the
	// happy path does not have to set setPolicy explicitly.
	if f.setPolicy.ID == "" {
		p := f.createPolicy
		p.CurrentVersion = version
		return p, nil
	}
	return f.setPolicy, nil
}

// fakePolicyVersions is an in-memory policyVersionStore recording the
// version rows it was asked to create and returning a canned history.
type fakePolicyVersions struct {
	createVersion store.PolicyVersion
	createErr     error
	listVersions  []store.PolicyVersion
	listErr       error

	gotCreates []store.CreatePolicyVersionParams
	gotListID  string
}

func (f *fakePolicyVersions) Create(_ context.Context, p store.CreatePolicyVersionParams) (store.PolicyVersion, error) {
	f.gotCreates = append(f.gotCreates, p)
	if f.createErr != nil {
		return store.PolicyVersion{}, f.createErr
	}
	// Echo the stored params back so publish tests can assert the response maps
	// digest/boundary/signing from what was written, without wiring a canned row.
	if f.createVersion.PolicyID == "" {
		return store.PolicyVersion{
			ID:          "pv-" + p.PolicyID,
			PolicyID:    p.PolicyID,
			Version:     p.Version,
			ContentJSON: p.ContentJSON,
			ContentHash: p.ContentHash,
			SigningJSON: p.SigningJSON,
			CreatedAt:   time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC),
		}, nil
	}
	return f.createVersion, nil
}

func (f *fakePolicyVersions) ListByPolicy(_ context.Context, policyID string) ([]store.PolicyVersion, error) {
	f.gotListID = policyID
	return f.listVersions, f.listErr
}

// GetByID satisfies the shared policyVersionStore seam (launchSession reads a
// pinned version back through it); the policy handlers never call it, so it is
// an unused stub here.
func (f *fakePolicyVersions) GetByID(context.Context, string) (store.PolicyVersion, error) {
	return store.PolicyVersion{}, nil
}

// setPolicyStores points the newPolicyStore/newPolicyVersionStore seams at the
// given fakes for the duration of the test, restoring the production factories
// afterwards. Because it mutates package-level vars, callers must not run in
// parallel.
func setPolicyStores(t *testing.T, ps policyStore, pvs policyVersionStore) {
	t.Helper()
	origP, origV := newPolicyStore, newPolicyVersionStore
	newPolicyStore = func(store.Querier) policyStore { return ps }
	newPolicyVersionStore = func(store.Querier) policyVersionStore { return pvs }
	t.Cleanup(func() {
		newPolicyStore = origP
		newPolicyVersionStore = origV
	})
}

// policyHarness wires a Deps (with a shared issuer/verifier key), points the
// store seams at the fakes, mounts the router, and returns a ready Authorization
// header for orgID. It reuses agentToken since only the org matters to these
// tests.
func policyHarness(t *testing.T, ps policyStore, pvs policyVersionStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setPolicyStores(t, ps, pvs)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

func storePolicy(id, org, name string, desc *string, version int, created time.Time) store.Policy {
	return store.Policy{
		ID:             id,
		OrgID:          org,
		Name:           name,
		Description:    desc,
		CurrentVersion: version,
		CreatedAt:      created,
	}
}

// TestListPoliciesPage proves the keyset lookahead: the handler asks the store
// for limit+1 rows, drops the extra, returns exactly limit items, and sets
// next_cursor from the last kept row.
func TestListPoliciesPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{listPolicies: []store.Policy{
		storePolicy("p1", "org-1", "one", nil, 1, t1),
		storePolicy("p2", "org-1", "two", strptr("second"), 3, t2),
		storePolicy("p3", "org-1", "three", nil, 1, t3), // the lookahead row
	}}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies?limit=2", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ps.gotListOrg != "org-1" {
		t.Errorf("list org = %q, want org-1", ps.gotListOrg)
	}
	if ps.gotListLimit != 3 {
		t.Errorf("list limit = %d, want 3 (limit+1 lookahead)", ps.gotListLimit)
	}

	var body apiPolicyList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[1].CurrentVersion != 3 || body.Items[1].Description == nil || *body.Items[1].Description != "second" {
		t.Errorf("second item mapped wrong: %+v", body.Items[1])
	}
	if body.NextCursor == nil {
		t.Fatal("next_cursor = nil, want a cursor for the last kept row")
	}
	cur, err := DecodeCursor(*body.NextCursor)
	if err != nil {
		t.Fatalf("decode next_cursor: %v", err)
	}
	if cur.ID != "p2" || !cur.CreatedAt.Equal(t2) {
		t.Errorf("next_cursor = (%v, %s), want (%v, p2)", cur.CreatedAt, cur.ID, t2)
	}
}

// TestListPoliciesLastPage proves a short page reports a null next_cursor and
// that an incoming cursor is decoded and forwarded to the store.
func TestListPoliciesLastPage(t *testing.T) {
	t1 := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{listPolicies: []store.Policy{
		storePolicy("p1", "org-1", "one", nil, 1, t1),
	}}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")

	cursor := EncodeCursor(Cursor{CreatedAt: t1, ID: "p0"})
	rec := doJSON(t, h, http.MethodGet, "/policies?limit=5&cursor="+cursor, "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ps.gotListAfterI != "p0" || !ps.gotListAfterC.Equal(t1) {
		t.Errorf("seek = (%v, %s), want (%v, p0)", ps.gotListAfterC, ps.gotListAfterI, t1)
	}

	var body apiPolicyList
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

// TestListPoliciesEmpty proves items serialises as [] (never null) when the org
// has no policies, and that the default limit drives a 51-row lookahead.
func TestListPoliciesEmpty(t *testing.T) {
	ps := &fakePolicyStore{listPolicies: nil}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got == "" || !containsItemsArray(got) {
		t.Errorf("body = %s, want items:[] present", got)
	}
	if ps.gotListLimit != 51 {
		t.Errorf("list limit = %d, want 51 (default 50 + lookahead)", ps.gotListLimit)
	}
}

func TestListPoliciesBadParams(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"non-integer limit", "/policies?limit=abc"},
		{"malformed cursor", "/policies?cursor=notacursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, hdr := policyHarness(t, &fakePolicyStore{}, &fakePolicyVersions{}, "org-1")
			rec := doJSON(t, h, http.MethodGet, tc.path, "", hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestCreatePolicy proves the happy path: the org comes from the token, name
// and description are forwarded, version 1 is created with the marshaled
// boundary and its sha256 digest (unsigned), the current-version pointer is set
// to 1, and the returned Policy reports current_version 1.
func TestCreatePolicy(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{
		createPolicy: storePolicy("p9", "org-1", "guardrail", strptr("keeps agents in"), 0, created),
	}
	pvs := &fakePolicyVersions{}
	h, hdr := policyHarness(t, ps, pvs, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/policies",
		`{"name":"guardrail","description":"keeps agents in","boundary":{"allowed_tools":["bash"],"network":"none"}}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if ps.gotCreateOrg != "org-1" {
		t.Errorf("create org = %q, want org-1 (from token)", ps.gotCreateOrg)
	}
	if ps.gotCreateName != "guardrail" {
		t.Errorf("create name = %q, want guardrail", ps.gotCreateName)
	}
	if ps.gotCreateDesc == nil || *ps.gotCreateDesc != "keeps agents in" {
		t.Errorf("create desc = %v, want keeps agents in", ps.gotCreateDesc)
	}

	// Exactly one version was created: version 1, with a sha256 digest over the
	// marshaled boundary and no signing metadata (unsigned at create).
	if len(pvs.gotCreates) != 1 {
		t.Fatalf("version creates = %d, want 1", len(pvs.gotCreates))
	}
	v := pvs.gotCreates[0]
	if v.PolicyID != "p9" {
		t.Errorf("version policy_id = %q, want p9", v.PolicyID)
	}
	if v.Version != 1 {
		t.Errorf("version = %d, want 1", v.Version)
	}
	if v.SigningJSON != nil {
		t.Errorf("signing_json = %s, want nil (unsigned at create)", v.SigningJSON)
	}
	wantHash := "sha256:" + sha256HexOf(t, v.ContentJSON)
	if v.ContentHash != wantHash {
		t.Errorf("content_hash = %q, want %q", v.ContentHash, wantHash)
	}
	// content_json is the canonical marshaled boundary.
	var gotBoundary apiPolicyBoundary
	if err := json.Unmarshal(v.ContentJSON, &gotBoundary); err != nil {
		t.Fatalf("decode content_json: %v", err)
	}
	if len(gotBoundary.AllowedTools) != 1 || gotBoundary.AllowedTools[0] != "bash" || gotBoundary.Network != "none" {
		t.Errorf("content_json boundary = %+v, want {[bash] none}", gotBoundary)
	}

	// current_version was advanced to 1, and the response reflects it.
	if ps.gotSetID != "p9" || ps.gotSetVersion != 1 {
		t.Errorf("set current version = (%q, %d), want (p9, 1)", ps.gotSetID, ps.gotSetVersion)
	}
	var body apiPolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != "p9" || body.Name != "guardrail" || body.CurrentVersion != 1 {
		t.Errorf("body = %+v, want id p9 name guardrail current_version 1", body)
	}
}

// TestCreatePolicyEmptyBoundary proves an empty (but present) boundary object is
// accepted — every boundary field is optional — and still yields version 1.
func TestCreatePolicyEmptyBoundary(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{createPolicy: storePolicy("p1", "org-1", "empty", nil, 0, created)}
	pvs := &fakePolicyVersions{}
	h, hdr := policyHarness(t, ps, pvs, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/policies", `{"name":"empty","boundary":{}}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if len(pvs.gotCreates) != 1 || string(pvs.gotCreates[0].ContentJSON) != "{}" {
		t.Errorf("content_json = %s, want {}", pvs.gotCreates[0].ContentJSON)
	}
}

func TestCreatePolicyValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing name", `{"boundary":{}}`, http.StatusBadRequest},
		{"empty name", `{"name":"","boundary":{}}`, http.StatusBadRequest},
		{"missing boundary", `{"name":"x"}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
		{"unknown field", `{"name":"x","boundary":{},"bogus":1}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, hdr := policyHarness(t, &fakePolicyStore{}, &fakePolicyVersions{}, "org-1")
			rec := doJSON(t, h, http.MethodPost, "/policies", tc.body, hdr)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.code, rec.Body.String())
			}
		})
	}
}

func TestCreatePolicyConflict(t *testing.T) {
	ps := &fakePolicyStore{createErr: store.ErrConflict}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")
	rec := doJSON(t, h, http.MethodPost, "/policies", `{"name":"dup","boundary":{}}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestGetPolicy(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p5", "org-1", "reader", nil, 2, created)}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p5", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ps.gotGetID != "p5" {
		t.Errorf("get id = %q, want p5", ps.gotGetID)
	}
	var body apiPolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.CurrentVersion != 2 {
		t.Errorf("current_version = %d, want 2", body.CurrentVersion)
	}
	if _, ok := rawKeys(t, rec.Body.Bytes())["description"]; ok {
		t.Error("response contains description; it must be omitted when nil")
	}
}

// TestGetPolicyCrossOrg proves a row owned by another org is reported as 404,
// not 403 (ADR-0002 §1).
func TestGetPolicyCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p5", "org-2", "other", nil, 1, created)}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p5", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestGetPolicyNotFound(t *testing.T) {
	ps := &fakePolicyStore{getErr: store.ErrNotFound}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/policies/missing", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestPublishPolicyVersion proves publish increments current_version, stores the
// supplied signing metadata alongside the boundary+digest, and maps the created
// version to the response (digest ← content_hash, boundary ← content_json,
// signing ← signing_json).
func TestPublishPolicyVersion(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p7", "org-1", "guardrail", nil, 3, created)}
	pvs := &fakePolicyVersions{}
	h, hdr := policyHarness(t, ps, pvs, "org-1")

	body := `{"boundary":{"network":"allowlist","fan_out_ceiling":4},` +
		`"signing":{"algorithm":"ed25519","key_id":"k1","signature":"c2ln"}}`
	rec := doJSON(t, h, http.MethodPost, "/policies/p7/versions", body, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// One version created at current_version+1 = 4, carrying the signing JSON.
	if len(pvs.gotCreates) != 1 {
		t.Fatalf("version creates = %d, want 1", len(pvs.gotCreates))
	}
	v := pvs.gotCreates[0]
	if v.PolicyID != "p7" || v.Version != 4 {
		t.Errorf("created version = (%q, %d), want (p7, 4)", v.PolicyID, v.Version)
	}
	if v.SigningJSON == nil {
		t.Fatal("signing_json = nil, want the supplied signing metadata")
	}
	var gotSigning apiSigningMetadata
	if err := json.Unmarshal(v.SigningJSON, &gotSigning); err != nil {
		t.Fatalf("decode signing_json: %v", err)
	}
	if gotSigning.Algorithm != "ed25519" || gotSigning.KeyID != "k1" || gotSigning.Signature != "c2ln" {
		t.Errorf("signing_json = %+v, want ed25519/k1/c2ln", gotSigning)
	}
	wantHash := "sha256:" + sha256HexOf(t, v.ContentJSON)
	if v.ContentHash != wantHash {
		t.Errorf("content_hash = %q, want %q", v.ContentHash, wantHash)
	}

	// current_version advanced to 4.
	if ps.gotSetID != "p7" || ps.gotSetVersion != 4 {
		t.Errorf("set current version = (%q, %d), want (p7, 4)", ps.gotSetID, ps.gotSetVersion)
	}

	// Response maps digest/boundary/signing from the stored row.
	var resp apiPolicyVersion
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PolicyID != "p7" || resp.Version != 4 || resp.Digest != wantHash {
		t.Errorf("resp = %+v, want policy_id p7 version 4 digest %s", resp, wantHash)
	}
	if len(resp.Signing) == 0 || string(resp.Signing) == "null" {
		t.Errorf("resp.signing = %s, want the signing object", resp.Signing)
	}
	if len(resp.Boundary) == 0 {
		t.Error("resp.boundary is empty, want the stored boundary")
	}
}

func TestPublishPolicyVersionValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing boundary", `{"signing":{"algorithm":"ed25519","key_id":"k1","signature":"s"}}`},
		{"missing signing", `{"boundary":{}}`},
		{"missing signing.algorithm", `{"boundary":{},"signing":{"key_id":"k1","signature":"s"}}`},
		{"missing signing.signature", `{"boundary":{},"signing":{"algorithm":"ed25519","key_id":"k1"}}`},
		{"malformed json", `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
			ps := &fakePolicyStore{getPolicy: storePolicy("p7", "org-1", "g", nil, 1, created)}
			h, hdr := policyHarness(t, ps, &fakePolicyVersions{}, "org-1")
			rec := doJSON(t, h, http.MethodPost, "/policies/p7/versions", tc.body, hdr)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestPublishPolicyVersionCrossOrg proves publishing against another org's
// policy id is a 404, and no version is created.
func TestPublishPolicyVersionCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p7", "org-2", "other", nil, 1, created)}
	pvs := &fakePolicyVersions{}
	h, hdr := policyHarness(t, ps, pvs, "org-1")

	body := `{"boundary":{},"signing":{"algorithm":"ed25519","key_id":"k1","signature":"s"}}`
	rec := doJSON(t, h, http.MethodPost, "/policies/p7/versions", body, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if len(pvs.gotCreates) != 0 {
		t.Errorf("version creates = %d, want 0 on cross-org", len(pvs.gotCreates))
	}
}

// TestPublishPolicyVersionConflict proves a duplicate (policy_id, version) — a
// concurrent publish racing for the same next number — surfaces as a 409.
func TestPublishPolicyVersionConflict(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p7", "org-1", "g", nil, 1, created)}
	pvs := &fakePolicyVersions{createErr: store.ErrConflict}
	h, hdr := policyHarness(t, ps, pvs, "org-1")

	body := `{"boundary":{},"signing":{"algorithm":"ed25519","key_id":"k1","signature":"s"}}`
	rec := doJSON(t, h, http.MethodPost, "/policies/p7/versions", body, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestListPolicyVersions proves the version history is confirmed same-org and
// mapped, including the unsigned version 1 whose signing renders as JSON null.
func TestListPolicyVersions(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p7", "org-1", "g", nil, 2, created)}
	pvs := &fakePolicyVersions{listVersions: []store.PolicyVersion{
		{ID: "v1", PolicyID: "p7", Version: 1, ContentJSON: json.RawMessage(`{}`), ContentHash: "sha256:aa", SigningJSON: nil, CreatedAt: created},
		{ID: "v2", PolicyID: "p7", Version: 2, ContentJSON: json.RawMessage(`{"network":"none"}`), ContentHash: "sha256:bb", SigningJSON: json.RawMessage(`{"algorithm":"ed25519","key_id":"k1","signature":"s"}`), CreatedAt: created},
	}}
	h, hdr := policyHarness(t, ps, pvs, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p7/versions", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if pvs.gotListID != "p7" {
		t.Errorf("list policy id = %q, want p7", pvs.gotListID)
	}

	var body apiPolicyVersionList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if body.Items[0].Version != 1 || body.Items[0].Digest != "sha256:aa" {
		t.Errorf("v1 mapped wrong: %+v", body.Items[0])
	}
	// The unsigned version 1 renders signing as JSON null (present, per schema).
	if string(body.Items[0].Signing) != "null" {
		t.Errorf("v1 signing = %s, want null", body.Items[0].Signing)
	}
	if string(body.Items[1].Signing) == "null" || len(body.Items[1].Signing) == 0 {
		t.Errorf("v2 signing = %s, want the signing object", body.Items[1].Signing)
	}
}

// TestListPolicyVersionsEmpty proves items serialises as [] (never null) for a
// policy with no versions and that the response is a plain {items} envelope
// (no next_cursor field).
func TestListPolicyVersionsEmpty(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p7", "org-1", "g", nil, 0, created)}
	h, hdr := policyHarness(t, ps, &fakePolicyVersions{listVersions: nil}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p7/versions", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !containsItemsArray(rec.Body.String()) {
		t.Errorf("body = %s, want items:[] present", rec.Body.String())
	}
	if _, ok := rawKeys(t, rec.Body.Bytes())["next_cursor"]; ok {
		t.Error("PolicyVersionList must not carry next_cursor")
	}
}

// TestListPolicyVersionsCrossOrg proves listing versions of another org's policy
// is a 404, and the version store is never consulted.
func TestListPolicyVersionsCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p7", "org-2", "other", nil, 1, created)}
	pvs := &fakePolicyVersions{}
	h, hdr := policyHarness(t, ps, pvs, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p7/versions", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if pvs.gotListID != "" {
		t.Errorf("list consulted with %q, want no call on cross-org", pvs.gotListID)
	}
}

// TestPolicyRoutesRequireAuth proves every policy route sits behind RequireAuth:
// a request with no token is a 401 and never reaches the store.
func TestPolicyRoutesRequireAuth(t *testing.T) {
	tests := []struct {
		method, path string
	}{
		{http.MethodGet, "/policies"},
		{http.MethodPost, "/policies"},
		{http.MethodGet, "/policies/p1"},
		{http.MethodGet, "/policies/p1/versions"},
		{http.MethodPost, "/policies/p1/versions"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setPolicyStores(t, &fakePolicyStore{}, &fakePolicyVersions{})
			h := mountRouter(deps)
			rec := doJSON(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}
