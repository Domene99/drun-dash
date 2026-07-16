package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakePolicyRoleStore is an in-memory policyRoleStore: ListRoles returns the
// stored set (or listErr) and SetRoles records its arguments and updates the
// stored set (or returns setErr), so the policy-roles handlers can be exercised
// without a database.
type fakePolicyRoleStore struct {
	roleIDs []string
	listErr error
	setErr  error

	setCalled     bool
	gotSetID      string
	gotSetRoleIDs []string
}

func (f *fakePolicyRoleStore) ListRoles(_ context.Context, _ string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.roleIDs, nil
}

func (f *fakePolicyRoleStore) SetRoles(_ context.Context, policyID string, roleIDs []string) error {
	f.setCalled = true
	f.gotSetID, f.gotSetRoleIDs = policyID, roleIDs
	if f.setErr != nil {
		return f.setErr
	}
	f.roleIDs = roleIDs
	return nil
}

// setPolicyRoleStore points the newPolicyRoleStore seam at prs for the test,
// restoring the production factory afterwards. It mutates a package-level var,
// so callers must not run in parallel.
func setPolicyRoleStore(t *testing.T, prs policyRoleStore) {
	t.Helper()
	orig := newPolicyRoleStore
	newPolicyRoleStore = func(store.Querier) policyRoleStore { return prs }
	t.Cleanup(func() { newPolicyRoleStore = orig })
}

// policyRolesHarness wires a Deps, points the policy / policy-role / role seams
// at the fakes, mounts the router, and returns an Authorization header for
// orgID. The default permissive resolver lets the policies.manage-gated PUT
// through; a test overrides it with setResolver to assert the gate.
func policyRolesHarness(t *testing.T, ps policyStore, prs policyRoleStore, rs roleStore, orgID string) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setPolicyStores(t, ps, &fakePolicyVersions{})
	setPolicyRoleStore(t, prs)
	setRoleStore(t, rs)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, orgID)}}
	return h, hdr
}

// TestGetPolicyRoles proves the happy path returns the policy's permitted role
// ids, after confirming the policy belongs to the caller's org.
func TestGetPolicyRoles(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-1", "guard", nil, 1, created)}
	prs := &fakePolicyRoleStore{roleIDs: []string{"r1", "r2"}}
	h, hdr := policyRolesHarness(t, ps, prs, &fakeRoleStore{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p1/roles", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body apiPolicyRoles
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.RoleIDs) != 2 || body.RoleIDs[0] != "r1" || body.RoleIDs[1] != "r2" {
		t.Errorf("role_ids = %v, want [r1 r2]", body.RoleIDs)
	}
}

// TestGetPolicyRolesEmpty proves an empty permitted set serialises as [] (never
// null).
func TestGetPolicyRolesEmpty(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-1", "guard", nil, 1, created)}
	prs := &fakePolicyRoleStore{roleIDs: nil}
	h, hdr := policyRolesHarness(t, ps, prs, &fakeRoleStore{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p1/roles", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !containsRoleIDsArray(rec.Body.String()) {
		t.Errorf("body = %s, want role_ids:[] present", rec.Body.String())
	}
}

// TestGetPolicyRolesCrossOrg proves a policy owned by another org is a 404 (not
// 403), and the policy-role store is never consulted.
func TestGetPolicyRolesCrossOrg(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-2", "other", nil, 1, created)}
	prs := &fakePolicyRoleStore{roleIDs: []string{"r1"}}
	h, hdr := policyRolesHarness(t, ps, prs, &fakeRoleStore{}, "org-1")

	rec := doJSON(t, h, http.MethodGet, "/policies/p1/roles", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestGetPolicyRolesNotFound(t *testing.T) {
	ps := &fakePolicyStore{getErr: store.ErrNotFound}
	h, hdr := policyRolesHarness(t, ps, &fakePolicyRoleStore{}, &fakeRoleStore{}, "org-1")
	rec := doJSON(t, h, http.MethodGet, "/policies/missing/roles", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestUpdatePolicyRoles proves the happy path: the caller's roles are validated
// same-org, the permitted set is replaced wholesale, and the stored set is
// returned.
func TestUpdatePolicyRoles(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-1", "guard", nil, 1, created)}
	prs := &fakePolicyRoleStore{}
	rs := &fakeRoleStore{byID: map[string]store.Role{
		"r1": storeRole("r1", "org-1", "Reviewers", false, false, store.PermAgentsRun),
		"r2": storeRole("r2", "org-1", "Ops", false, false, store.PermAgentsRun),
	}}
	h, hdr := policyRolesHarness(t, ps, prs, rs, "org-1")

	rec := doJSON(t, h, http.MethodPut, "/policies/p1/roles", `{"role_ids":["r1","r2"]}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !prs.setCalled || prs.gotSetID != "p1" {
		t.Errorf("SetRoles called=%v id=%q, want true p1", prs.setCalled, prs.gotSetID)
	}
	if len(prs.gotSetRoleIDs) != 2 || prs.gotSetRoleIDs[0] != "r1" || prs.gotSetRoleIDs[1] != "r2" {
		t.Errorf("SetRoles role_ids = %v, want [r1 r2]", prs.gotSetRoleIDs)
	}
	var body apiPolicyRoles
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.RoleIDs) != 2 {
		t.Errorf("response role_ids = %v, want the stored set", body.RoleIDs)
	}
}

// TestUpdatePolicyRolesEmpties proves clearing the permitted set (empty
// role_ids) is accepted and writes an empty set.
func TestUpdatePolicyRolesEmpties(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-1", "guard", nil, 1, created)}
	prs := &fakePolicyRoleStore{roleIDs: []string{"r1"}}
	h, hdr := policyRolesHarness(t, ps, prs, &fakeRoleStore{}, "org-1")

	rec := doJSON(t, h, http.MethodPut, "/policies/p1/roles", `{"role_ids":[]}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !prs.setCalled || len(prs.gotSetRoleIDs) != 0 {
		t.Errorf("SetRoles called=%v role_ids=%v, want true []", prs.setCalled, prs.gotSetRoleIDs)
	}
	if !containsRoleIDsArray(rec.Body.String()) {
		t.Errorf("body = %s, want role_ids:[] present", rec.Body.String())
	}
}

// TestUpdatePolicyRolesForbidden proves the PUT is gated on policies.manage: a
// caller without it is a 403 and the set is never written.
func TestUpdatePolicyRolesForbidden(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-1", "guard", nil, 1, created)}
	prs := &fakePolicyRoleStore{}
	h, hdr := policyRolesHarness(t, ps, prs, &fakeRoleStore{}, "org-1")
	setResolver(t, permResolver(store.PermAgentsRun)) // not policies.manage

	rec := doJSON(t, h, http.MethodPut, "/policies/p1/roles", `{"role_ids":["r1"]}`, hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if prs.setCalled {
		t.Error("SetRoles called despite a permission miss")
	}
}

// TestUpdatePolicyRolesCrossOrgRole proves a role that belongs to another org is
// rejected as a 404 and the set is never written (no cross-org role leaks in).
func TestUpdatePolicyRolesCrossOrgRole(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-1", "guard", nil, 1, created)}
	prs := &fakePolicyRoleStore{}
	rs := &fakeRoleStore{byID: map[string]store.Role{
		"r1": storeRole("r1", "org-2", "Other", false, false),
	}}
	h, hdr := policyRolesHarness(t, ps, prs, rs, "org-1")

	rec := doJSON(t, h, http.MethodPut, "/policies/p1/roles", `{"role_ids":["r1"]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if prs.setCalled {
		t.Error("SetRoles called despite a cross-org role")
	}
}

// TestUpdatePolicyRolesUnknownRole proves an unknown role id is a 404 and the
// set is never written.
func TestUpdatePolicyRolesUnknownRole(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-1", "guard", nil, 1, created)}
	prs := &fakePolicyRoleStore{}
	rs := &fakeRoleStore{} // GetByID → ErrNotFound for any id
	h, hdr := policyRolesHarness(t, ps, prs, rs, "org-1")

	rec := doJSON(t, h, http.MethodPut, "/policies/p1/roles", `{"role_ids":["ghost"]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if prs.setCalled {
		t.Error("SetRoles called despite an unknown role")
	}
}

// TestUpdatePolicyRolesCrossOrgPolicy proves writing roles on another org's
// policy is a 404 and the set is never written.
func TestUpdatePolicyRolesCrossOrgPolicy(t *testing.T) {
	created := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	ps := &fakePolicyStore{getPolicy: storePolicy("p1", "org-2", "other", nil, 1, created)}
	prs := &fakePolicyRoleStore{}
	h, hdr := policyRolesHarness(t, ps, prs, &fakeRoleStore{}, "org-1")

	rec := doJSON(t, h, http.MethodPut, "/policies/p1/roles", `{"role_ids":["r1"]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if prs.setCalled {
		t.Error("SetRoles called on a cross-org policy")
	}
}

// TestPolicyRolesRoutesRequireAuth proves both policy-roles routes sit behind
// RequireAuth: a request with no token is a 401.
func TestPolicyRolesRoutesRequireAuth(t *testing.T) {
	tests := []struct{ method, path string }{
		{http.MethodGet, "/policies/p1/roles"},
		{http.MethodPut, "/policies/p1/roles"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setPolicyStores(t, &fakePolicyStore{}, &fakePolicyVersions{})
			setPolicyRoleStore(t, &fakePolicyRoleStore{})
			setRoleStore(t, &fakeRoleStore{})
			h := mountRouter(deps)
			rec := doJSON(t, h, tc.method, tc.path, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// containsRoleIDsArray reports whether body carries a present role_ids array
// field, mirroring containsItemsArray for the list envelopes.
func containsRoleIDsArray(body string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return false
	}
	ids, ok := m["role_ids"]
	return ok && string(ids) == "[]"
}
