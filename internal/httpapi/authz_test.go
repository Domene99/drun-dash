package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// fakeResolver is an in-memory accessResolver: it answers permission and
// run-authorization checks from canned data and records the arguments each
// method was called with, so the authorization layer can be exercised without a
// database. A zero fakeResolver denies everything (fail-closed) unless allowAll
// is set or specific perms are populated.
type fakeResolver struct {
	allowAll  bool
	perms     map[store.Permission]bool
	effective []store.Permission
	hasErr    error
	effErr    error

	runGrant store.RunGrant
	runOK    bool
	runErr   error

	// accessible backs ListAccessibleAgentIDs (GET /me/agents): the ids the
	// caller may see, or accessibleErr on failure.
	accessible    []string
	accessibleErr error

	gotMemberID string
	gotAgentID  string
	gotOrgID    string
	gotPerm     store.Permission
}

func (f *fakeResolver) HasPermission(_ context.Context, memberID string, perm store.Permission) (bool, error) {
	f.gotMemberID, f.gotPerm = memberID, perm
	if f.hasErr != nil {
		return false, f.hasErr
	}
	if f.allowAll {
		return true, nil
	}
	return f.perms[perm], nil
}

func (f *fakeResolver) EffectivePermissions(_ context.Context, memberID string) ([]store.Permission, error) {
	f.gotMemberID = memberID
	if f.effErr != nil {
		return nil, f.effErr
	}
	if f.effective != nil {
		return f.effective, nil
	}
	if f.allowAll {
		return store.AllPermissions(), nil
	}
	var out []store.Permission
	for _, p := range store.AllPermissions() {
		if f.perms[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeResolver) AuthorizeRun(_ context.Context, memberID, agentID string) (store.RunGrant, bool, error) {
	f.gotMemberID, f.gotAgentID = memberID, agentID
	if f.runErr != nil {
		return store.RunGrant{}, false, f.runErr
	}
	return f.runGrant, f.runOK, nil
}

func (f *fakeResolver) ListAccessibleAgentIDs(_ context.Context, memberID, orgID string) ([]string, error) {
	f.gotMemberID, f.gotOrgID = memberID, orgID
	if f.accessibleErr != nil {
		return nil, f.accessibleErr
	}
	return f.accessible, nil
}

// allowAllResolver returns a resolver that grants every permission and
// authorizes every run — the permissive default the harness installs so the
// pre-RBAC handler tests keep exercising their own concerns rather than
// authorization.
func allowAllResolver() *fakeResolver { return &fakeResolver{allowAll: true, runOK: true} }

// permResolver returns a resolver that grants exactly the listed permissions.
func permResolver(perms ...store.Permission) *fakeResolver {
	m := make(map[store.Permission]bool, len(perms))
	for _, p := range perms {
		m[p] = true
	}
	return &fakeResolver{perms: m}
}

// setResolver points the newAccessResolver seam at res for the duration of the
// test, restoring the prior factory afterwards. Because it mutates a
// package-level var, callers must not run in parallel.
func setResolver(t *testing.T, res accessResolver) {
	t.Helper()
	orig := newAccessResolver
	newAccessResolver = func(store.Querier) accessResolver { return res }
	t.Cleanup(func() { newAccessResolver = orig })
}

// init installs the permissive resolver as the package default for the whole
// test binary, so handler tests that predate RBAC (and never set up grants)
// still pass through the permission gates. Tests asserting a 403/500 override it
// with setResolver.
func init() {
	newAccessResolver = func(store.Querier) accessResolver { return allowAllResolver() }
}

// TestRequirePermissionForbidden proves a mutating endpoint answers 403 when the
// caller lacks the required permission, and never reaches the store.
func TestRequirePermissionForbidden(t *testing.T) {
	fake := &fakeAgentStore{}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAgentStore(t, fake)
	setResolver(t, permResolver(store.PermPoliciesManage)) // has a permission, but not agents.manage
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/agents", `{"name":"x"}`, hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotCreateName != "" {
		t.Error("store reached despite a permission miss")
	}
}

// TestRequirePermissionGranted proves the gate lets a caller with the required
// permission through to the handler.
func TestRequirePermissionGranted(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	fake := &fakeAgentStore{createAgent: storeAgent("a1", "org-1", "x", nil, 0, created)}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAgentStore(t, fake)
	setResolver(t, permResolver(store.PermAgentsManage))
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/agents", `{"name":"x"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotCreateName != "x" {
		t.Error("handler did not reach the store")
	}
}

// TestRequirePermissionResolverError proves a resolver failure fails closed as a
// 500 (never letting the request through).
func TestRequirePermissionResolverError(t *testing.T) {
	fake := &fakeAgentStore{}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAgentStore(t, fake)
	setResolver(t, &fakeResolver{hasErr: context.DeadlineExceeded})
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodDelete, "/agents/a1", "", hdr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotDeleteID != "" {
		t.Error("store reached despite a resolver error")
	}
}

// TestRequireAnyPermission proves GET /members admits a caller holding either
// members.manage or audit.view, and rejects one holding neither.
func TestRequireAnyPermission(t *testing.T) {
	tests := []struct {
		name string
		res  *fakeResolver
		want int
	}{
		{"members.manage", permResolver(store.PermMembersManage), http.StatusOK},
		{"audit.view", permResolver(store.PermAuditView), http.StatusOK},
		{"neither", permResolver(store.PermAgentsManage), http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			setMemberDirectory(t, &fakeMemberDirectory{})
			setMemberRoleStore(t, &fakeMemberRoleStore{})
			setResolver(t, tc.res)
			h := mountRouter(deps)
			hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

			rec := doJSON(t, h, http.MethodGet, "/members", "", hdr)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestWhoamiSurfacesPermissions proves /auth/whoami returns the caller's
// effective permission set from the resolver.
func TestWhoamiSurfacesPermissions(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setResolver(t, permResolver(store.PermAuditView, store.PermAgentsRun))
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodGet, "/auth/whoami", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var id identityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]bool{}
	for _, p := range id.Permissions {
		got[p] = true
	}
	if !got["audit.view"] || !got["agents.run"] {
		t.Errorf("permissions = %v, want audit.view and agents.run", id.Permissions)
	}
	if got["roles.manage"] {
		t.Errorf("permissions = %v, want no roles.manage", id.Permissions)
	}
}
