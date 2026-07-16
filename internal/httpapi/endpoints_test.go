package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
	"github.com/Domene99/drun-dash/internal/vault"
)

// dbRef is a well-formed vault://db/<uuid> ref used across the endpoint tests.
const dbRef = "vault://db/11111111-1111-4111-8111-111111111111"

// fakeModelEndpointStore is an in-memory modelEndpointStore recording arguments
// and returning canned rows/errors, so the endpoint handlers can be exercised
// without a database.
type fakeModelEndpointStore struct {
	createEP  store.ModelEndpoint
	createErr error
	getEP     store.ModelEndpoint
	getErr    error
	listEPs   []store.ModelEndpoint
	listErr   error
	updateEP  store.ModelEndpoint
	updateErr error
	activeEP  store.ModelEndpoint
	activeErr error
	deleteErr error

	gotCreate      store.CreateModelEndpointParams
	gotUpdate      store.UpdateModelEndpointParams
	gotGetID       string
	gotListOrg     string
	gotSetActiveID string
	gotSetActive   bool
	gotDeleteID    string
	createCalled   bool
	updateCalled   bool
	deleteCalled   bool
}

func (f *fakeModelEndpointStore) Create(_ context.Context, p store.CreateModelEndpointParams) (store.ModelEndpoint, error) {
	f.createCalled, f.gotCreate = true, p
	return f.createEP, f.createErr
}

func (f *fakeModelEndpointStore) GetByID(_ context.Context, id string) (store.ModelEndpoint, error) {
	f.gotGetID = id
	return f.getEP, f.getErr
}

func (f *fakeModelEndpointStore) ListByOrg(_ context.Context, orgID string) ([]store.ModelEndpoint, error) {
	f.gotListOrg = orgID
	return f.listEPs, f.listErr
}

func (f *fakeModelEndpointStore) Update(_ context.Context, p store.UpdateModelEndpointParams) (store.ModelEndpoint, error) {
	f.updateCalled, f.gotUpdate = true, p
	return f.updateEP, f.updateErr
}

func (f *fakeModelEndpointStore) SetActive(_ context.Context, id string, active bool) (store.ModelEndpoint, error) {
	f.gotSetActiveID, f.gotSetActive = id, active
	return f.activeEP, f.activeErr
}

func (f *fakeModelEndpointStore) Delete(_ context.Context, id string) error {
	f.deleteCalled, f.gotDeleteID = true, id
	return f.deleteErr
}

// fakeSecretVault is an in-memory secretVault recording seal/reseal/delete
// calls and returning canned refs/errors.
type fakeSecretVault struct {
	sealRef   string
	sealErr   error
	resealErr error
	deleteErr error

	sealCalls      int
	resealCalls    int
	deleteCalls    int
	gotSealOrg     string
	gotSealPlain   string
	gotResealRef   string
	gotResealPlain string
	gotDeleteRef   string
}

func (f *fakeSecretVault) Seal(_ context.Context, orgID, plaintext string) (string, error) {
	f.sealCalls++
	f.gotSealOrg, f.gotSealPlain = orgID, plaintext
	if f.sealErr != nil {
		return "", f.sealErr
	}
	return f.sealRef, nil
}

func (f *fakeSecretVault) Reseal(_ context.Context, secretRef, plaintext string) error {
	f.resealCalls++
	f.gotResealRef, f.gotResealPlain = secretRef, plaintext
	return f.resealErr
}

func (f *fakeSecretVault) Delete(_ context.Context, secretRef string) error {
	f.deleteCalls++
	f.gotDeleteRef = secretRef
	return f.deleteErr
}

// setModelEndpointStore points the newModelEndpointStore seam at s for the test.
func setModelEndpointStore(t *testing.T, s modelEndpointStore) {
	t.Helper()
	orig := newModelEndpointStore
	newModelEndpointStore = func(store.Querier) modelEndpointStore { return s }
	t.Cleanup(func() { newModelEndpointStore = orig })
}

// endpointHarness wires a Deps with the given store and vault fakes, mounts the
// router, and returns a ready Authorization header for orgID.
func endpointHarness(t *testing.T, fs modelEndpointStore, fv secretVault, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	deps.Vault = fv
	setModelEndpointStore(t, fs)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

func storeEndpoint(id, org, name, secretRef string) store.ModelEndpoint {
	return store.ModelEndpoint{
		ID:        id,
		OrgID:     org,
		Name:      name,
		BaseURL:   "https://api.example.com",
		Provider:  "anthropic",
		SecretRef: secretRef,
		Active:    true,
		CreatedAt: time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC),
	}
}

// assertNoKeyLeak fails if the response body carries a raw key or the opaque
// secret_ref, and confirms secret_set is present.
func assertNoKeyLeak(t *testing.T, body []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, body)
	}
	for _, forbidden := range []string{"secret_ref", "api_key", "key"} {
		if _, ok := raw[forbidden]; ok {
			t.Errorf("response leaked %q: %s", forbidden, body)
		}
	}
	if _, ok := raw["secret_set"]; !ok {
		t.Errorf("response missing secret_set: %s", body)
	}
}

func TestCreateModelEndpointSealsKey(t *testing.T) {
	fs := &fakeModelEndpointStore{createEP: storeEndpoint("ep-1", "org-1", "anthropic-prod", dbRef)}
	fv := &fakeSecretVault{sealRef: dbRef}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/model-endpoints",
		`{"name":"anthropic-prod","provider":"anthropic","base_url":"https://api.example.com","api_key":"sk-secret"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if fv.sealCalls != 1 || fv.gotSealOrg != "org-1" || fv.gotSealPlain != "sk-secret" {
		t.Fatalf("Seal not called with (org-1, sk-secret): calls=%d org=%q", fv.sealCalls, fv.gotSealOrg)
	}
	if fs.gotCreate.OrgID != "org-1" || fs.gotCreate.SecretRef != dbRef {
		t.Fatalf("Create params = %+v, want org-1 with sealed secret_ref", fs.gotCreate)
	}

	assertNoKeyLeak(t, rec.Body.Bytes())
	var ep apiModelEndpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &ep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ep.SecretSet {
		t.Error("secret_set = false, want true when a key was sealed")
	}
}

func TestCreateModelEndpointNoKey(t *testing.T) {
	fs := &fakeModelEndpointStore{createEP: storeEndpoint("ep-1", "org-1", "no-key", "")}
	fv := &fakeSecretVault{}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/model-endpoints",
		`{"name":"no-key","provider":"anthropic","base_url":"https://api.example.com"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if fv.sealCalls != 0 {
		t.Errorf("Seal called %d times, want 0 for an empty api_key", fv.sealCalls)
	}
	if fs.gotCreate.SecretRef != "" {
		t.Errorf("Create secret_ref = %q, want empty", fs.gotCreate.SecretRef)
	}
	var ep apiModelEndpoint
	_ = json.Unmarshal(rec.Body.Bytes(), &ep)
	if ep.SecretSet {
		t.Error("secret_set = true, want false when no key was supplied")
	}
}

func TestCreateModelEndpointDuplicate(t *testing.T) {
	fs := &fakeModelEndpointStore{createErr: store.ErrConflict}
	fv := &fakeSecretVault{sealRef: dbRef}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/model-endpoints",
		`{"name":"dup","provider":"anthropic","base_url":"https://api.example.com","api_key":"sk"}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	// The sealed orphan is cleaned up when the insert conflicts.
	if fv.deleteCalls != 1 || fv.gotDeleteRef != dbRef {
		t.Errorf("orphan secret not cleaned up: deleteCalls=%d ref=%q", fv.deleteCalls, fv.gotDeleteRef)
	}
}

func TestCreateModelEndpointKeyWithoutVaultKey(t *testing.T) {
	fs := &fakeModelEndpointStore{}
	fv := &fakeSecretVault{sealErr: vault.ErrKeyNotConfigured}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/model-endpoints",
		`{"name":"x","provider":"anthropic","base_url":"https://api.example.com","api_key":"sk"}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if fs.createCalled {
		t.Error("store.Create reached despite a vault seal failure")
	}
}

func TestCreateModelEndpointValidation(t *testing.T) {
	fs := &fakeModelEndpointStore{}
	h, hdr := endpointHarness(t, fs, &fakeSecretVault{}, "org-1")

	rec := doJSON(t, h, http.MethodPost, "/model-endpoints",
		`{"provider":"anthropic","base_url":"https://api.example.com"}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing name (body %s)", rec.Code, rec.Body.String())
	}
	if fs.createCalled {
		t.Error("store.Create reached despite validation failure")
	}
}

func TestListModelEndpoints(t *testing.T) {
	fs := &fakeModelEndpointStore{listEPs: []store.ModelEndpoint{
		storeEndpoint("ep-1", "org-1", "a", dbRef),
		storeEndpoint("ep-2", "org-1", "b", ""),
	}}
	h, hdr := endpointHarness(t, fs, &fakeSecretVault{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/model-endpoints", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fs.gotListOrg != "org-1" {
		t.Errorf("ListByOrg org = %q, want org-1", fs.gotListOrg)
	}
	var list apiModelEndpointList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Items) != 2 || !list.Items[0].SecretSet || list.Items[1].SecretSet {
		t.Fatalf("items mapped wrong: %+v", list.Items)
	}
}

func TestGetModelEndpointCrossOrg(t *testing.T) {
	fs := &fakeModelEndpointStore{getEP: storeEndpoint("ep-1", "org-2", "other", dbRef)}
	h, hdr := endpointHarness(t, fs, &fakeSecretVault{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/model-endpoints/ep-1", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a cross-org endpoint (body %s)", rec.Code, rec.Body.String())
	}
}

func TestPatchModelEndpointReseals(t *testing.T) {
	fs := &fakeModelEndpointStore{
		getEP:    storeEndpoint("ep-1", "org-1", "anthropic", dbRef),
		updateEP: storeEndpoint("ep-1", "org-1", "anthropic", dbRef),
	}
	fv := &fakeSecretVault{}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodPatch, "/model-endpoints/ep-1", `{"api_key":"sk-rotated"}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// An existing db-backed ref rotates in place via Reseal (not a new Seal).
	if fv.resealCalls != 1 || fv.gotResealRef != dbRef || fv.gotResealPlain != "sk-rotated" {
		t.Fatalf("Reseal not called correctly: calls=%d ref=%q", fv.resealCalls, fv.gotResealRef)
	}
	if fv.sealCalls != 0 {
		t.Errorf("Seal called %d times, want 0 (ref already db-backed)", fv.sealCalls)
	}
	if !fs.updateCalled || fs.gotUpdate.SecretRef != dbRef {
		t.Fatalf("Update params = %+v, want secret_ref preserved", fs.gotUpdate)
	}
	assertNoKeyLeak(t, rec.Body.Bytes())
}

func TestPatchModelEndpointSealsWhenNoExistingRef(t *testing.T) {
	fs := &fakeModelEndpointStore{
		getEP:    storeEndpoint("ep-1", "org-1", "anthropic", ""),
		updateEP: storeEndpoint("ep-1", "org-1", "anthropic", dbRef),
	}
	fv := &fakeSecretVault{sealRef: dbRef}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodPatch, "/model-endpoints/ep-1", `{"api_key":"sk-new"}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fv.sealCalls != 1 || fv.resealCalls != 0 {
		t.Fatalf("want a fresh Seal (no existing ref): seal=%d reseal=%d", fv.sealCalls, fv.resealCalls)
	}
	if fs.gotUpdate.SecretRef != dbRef {
		t.Errorf("Update secret_ref = %q, want the freshly sealed ref", fs.gotUpdate.SecretRef)
	}
}

func TestPatchModelEndpointTogglesActive(t *testing.T) {
	fs := &fakeModelEndpointStore{
		getEP:    storeEndpoint("ep-1", "org-1", "anthropic", ""),
		updateEP: storeEndpoint("ep-1", "org-1", "anthropic", ""),
		activeEP: storeEndpoint("ep-1", "org-1", "anthropic", ""),
	}
	h, hdr := endpointHarness(t, fs, &fakeSecretVault{}, "org-1")

	rec := doJSON(t, h, http.MethodPatch, "/model-endpoints/ep-1", `{"active":false}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fs.gotSetActiveID != "ep-1" || fs.gotSetActive != false {
		t.Fatalf("SetActive(%q, %v), want (ep-1, false)", fs.gotSetActiveID, fs.gotSetActive)
	}
}

func TestDeleteModelEndpointRemovesSecret(t *testing.T) {
	fs := &fakeModelEndpointStore{getEP: storeEndpoint("ep-1", "org-1", "anthropic", dbRef)}
	fv := &fakeSecretVault{}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodDelete, "/model-endpoints/ep-1", "", hdr)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if !fs.deleteCalled || fs.gotDeleteID != "ep-1" {
		t.Errorf("store.Delete not called for ep-1")
	}
	if fv.deleteCalls != 1 || fv.gotDeleteRef != dbRef {
		t.Errorf("backing secret not deleted: calls=%d ref=%q", fv.deleteCalls, fv.gotDeleteRef)
	}
}

func TestDeleteModelEndpointWithoutDBRefSkipsVault(t *testing.T) {
	fs := &fakeModelEndpointStore{getEP: storeEndpoint("ep-1", "org-1", "dev", "vault://dev/anthropic")}
	fv := &fakeSecretVault{}
	h, hdr := endpointHarness(t, fs, fv, "org-1")

	rec := doJSON(t, h, http.MethodDelete, "/model-endpoints/ep-1", "", hdr)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if fv.deleteCalls != 0 {
		t.Errorf("vault.Delete called %d times for a non-db ref, want 0", fv.deleteCalls)
	}
}

func TestCreateModelEndpointForbidden(t *testing.T) {
	fs := &fakeModelEndpointStore{}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	deps.Vault = &fakeSecretVault{}
	setModelEndpointStore(t, fs)
	setResolver(t, permResolver(store.PermAgentsManage)) // has a permission, but not endpoints.manage
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/model-endpoints",
		`{"name":"x","provider":"anthropic","base_url":"https://api.example.com"}`, hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if fs.createCalled {
		t.Error("store reached despite a permission miss")
	}
}
