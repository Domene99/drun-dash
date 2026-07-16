package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// ---- fakes ----------------------------------------------------------------

type fakeRoleStore struct {
	roles     []store.Role
	byID      map[string]store.Role
	createRet store.Role
	createErr error
	updateErr error
	setPermsErr,
	setAgentsErr,
	deleteErr error

	gotCreate    store.CreateRoleParams
	gotUpdate    store.UpdateRoleParams
	gotSetPerms  []store.Permission
	gotSetAgents []store.RoleAgentGrant
	gotDeleteID  string
}

func (f *fakeRoleStore) ListByOrg(_ context.Context, _ string) ([]store.Role, error) {
	return f.roles, nil
}

func (f *fakeRoleStore) Create(_ context.Context, p store.CreateRoleParams) (store.Role, error) {
	f.gotCreate = p
	if f.createErr != nil {
		return store.Role{}, f.createErr
	}
	return f.createRet, nil
}

func (f *fakeRoleStore) GetByID(_ context.Context, id string) (store.Role, error) {
	if r, ok := f.byID[id]; ok {
		return r, nil
	}
	return store.Role{}, store.ErrNotFound
}

func (f *fakeRoleStore) Update(_ context.Context, id string, p store.UpdateRoleParams) (store.Role, error) {
	f.gotUpdate = p
	if f.updateErr != nil {
		return store.Role{}, f.updateErr
	}
	r := f.byID[id]
	r.Name, r.Description, r.AllAgents = p.Name, p.Description, p.AllAgents
	return r, nil
}

func (f *fakeRoleStore) SetPermissions(_ context.Context, _ string, perms []store.Permission) error {
	f.gotSetPerms = perms
	return f.setPermsErr
}

func (f *fakeRoleStore) SetAgents(_ context.Context, _ string, agents []store.RoleAgentGrant) error {
	f.gotSetAgents = agents
	return f.setAgentsErr
}

func (f *fakeRoleStore) Delete(_ context.Context, id string) error {
	f.gotDeleteID = id
	return f.deleteErr
}

func setRoleStore(t *testing.T, s roleStore) {
	t.Helper()
	orig := newRoleStore
	newRoleStore = func(store.Querier) roleStore { return s }
	t.Cleanup(func() { newRoleStore = orig })
}

type fakeMemberDirectory struct {
	members []store.Member
	byID    map[string]store.Member
	listErr error
}

func (f *fakeMemberDirectory) GetByID(_ context.Context, id string) (store.Member, error) {
	if m, ok := f.byID[id]; ok {
		return m, nil
	}
	return store.Member{}, store.ErrNotFound
}

func (f *fakeMemberDirectory) ListByOrg(_ context.Context, _ string) ([]store.Member, error) {
	return f.members, f.listErr
}

func setMemberDirectory(t *testing.T, s memberDirectory) {
	t.Helper()
	orig := newMemberDirectory
	newMemberDirectory = func(store.Querier) memberDirectory { return s }
	t.Cleanup(func() { newMemberDirectory = orig })
}

type roleAssignCall struct{ member, role string }

type fakeMemberRoleStore struct {
	rolesByMember map[string][]store.Role
	assignErr     error
	removeErr     error

	gotAssigned []roleAssignCall
	gotRemoved  []roleAssignCall
}

func (f *fakeMemberRoleStore) AssignRole(_ context.Context, memberID, roleID string) (store.MemberRoleAssignment, error) {
	f.gotAssigned = append(f.gotAssigned, roleAssignCall{memberID, roleID})
	if f.assignErr != nil {
		return store.MemberRoleAssignment{}, f.assignErr
	}
	return store.MemberRoleAssignment{MemberID: memberID, RoleID: roleID}, nil
}

func (f *fakeMemberRoleStore) RemoveRole(_ context.Context, memberID, roleID string) error {
	f.gotRemoved = append(f.gotRemoved, roleAssignCall{memberID, roleID})
	return f.removeErr
}

func (f *fakeMemberRoleStore) ListRolesForMember(_ context.Context, memberID string) ([]store.Role, error) {
	return f.rolesByMember[memberID], nil
}

func setMemberRoleStore(t *testing.T, s memberRoleStore) {
	t.Helper()
	orig := newMemberRoleStore
	newMemberRoleStore = func(store.Querier) memberRoleStore { return s }
	t.Cleanup(func() { newMemberRoleStore = orig })
}

func storeRole(id, org, name string, builtin, allAgents bool, perms ...store.Permission) store.Role {
	return store.Role{
		ID:          id,
		OrgID:       org,
		Name:        name,
		Builtin:     builtin,
		AllAgents:   allAgents,
		Permissions: perms,
		CreatedAt:   time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC),
	}
}

// rolesHarness mounts the router with the given role store and an
// authorization header for member-1 in org-1. The default permissive resolver
// grants roles.manage, so the gate lets these tests exercise the handler.
func rolesHarness(t *testing.T, rs roleStore) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setRoleStore(t, rs)
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}
	return h, hdr
}

// ---- roles ----------------------------------------------------------------

func TestListRoles(t *testing.T) {
	rs := &fakeRoleStore{roles: []store.Role{
		storeRole("r1", "org-1", "Owner", true, true, store.PermRolesManage),
		storeRole("r2", "org-1", "Custom", false, false, store.PermAgentsRun),
	}}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodGet, "/roles", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body apiRoleList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if !body.Items[0].Builtin || body.Items[0].Name != "Owner" {
		t.Errorf("first role = %+v, want builtin Owner", body.Items[0])
	}
	// Permissions must serialise as a present array.
	if body.Items[1].Permissions == nil {
		t.Error("permissions serialised as null, want []")
	}
}

func TestCreateRole(t *testing.T) {
	rs := &fakeRoleStore{createRet: storeRole("r9", "org-1", "Reviewers", false, false, store.PermAgentsRun)}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodPost, "/roles",
		`{"name":"Reviewers","permissions":["agents.run"]}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if rs.gotCreate.OrgID != "org-1" {
		t.Errorf("create org = %q, want org-1 (from token)", rs.gotCreate.OrgID)
	}
	if rs.gotCreate.Builtin {
		t.Error("created role must be custom (builtin=false)")
	}
	if len(rs.gotCreate.Permissions) != 1 || rs.gotCreate.Permissions[0] != store.PermAgentsRun {
		t.Errorf("create permissions = %v, want [agents.run]", rs.gotCreate.Permissions)
	}
}

func TestCreateRoleInvalidPermission(t *testing.T) {
	rs := &fakeRoleStore{createErr: store.ErrInvalidPermission}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodPost, "/roles",
		`{"name":"Bad","permissions":["does.not.exist"]}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCreateRoleConflict(t *testing.T) {
	rs := &fakeRoleStore{createErr: store.ErrConflict}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodPost, "/roles", `{"name":"Owner"}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCreateRoleUnknownAgent(t *testing.T) {
	rs := &fakeRoleStore{}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setRoleStore(t, rs)
	// Agent lives in another org → the grant is rejected as an unknown agent.
	setAgentStore(t, &fakeAgentStore{getAgent: storeAgent("a1", "org-2", "other", nil, 0, time.Now())})
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/roles",
		`{"name":"X","agents":[{"agent_id":"a1"}]}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if rs.gotCreate.Name != "" {
		t.Error("role created despite an invalid agent grant")
	}
}

func TestGetRoleCrossOrg(t *testing.T) {
	rs := &fakeRoleStore{byID: map[string]store.Role{
		"r1": storeRole("r1", "org-2", "Other", false, false),
	}}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodGet, "/roles/r1", "", hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateRoleReplacesSets(t *testing.T) {
	rs := &fakeRoleStore{byID: map[string]store.Role{
		"r2": storeRole("r2", "org-1", "Custom", false, false, store.PermAgentsRun),
	}}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodPatch, "/roles/r2",
		`{"description":"updated","permissions":["audit.view"]}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rs.gotSetPerms == nil || len(rs.gotSetPerms) != 1 || rs.gotSetPerms[0] != store.PermAuditView {
		t.Errorf("SetPermissions = %v, want [audit.view]", rs.gotSetPerms)
	}
	// Agents omitted → SetAgents must not be called.
	if rs.gotSetAgents != nil {
		t.Error("SetAgents called though agents were omitted")
	}
}

func TestUpdateRoleBuiltinRename(t *testing.T) {
	rs := &fakeRoleStore{
		byID:      map[string]store.Role{"r1": storeRole("r1", "org-1", "Owner", true, true)},
		updateErr: store.ErrImmutableBuiltin,
	}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodPatch, "/roles/r1", `{"name":"NotOwner"}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestDeleteRoleBuiltin(t *testing.T) {
	rs := &fakeRoleStore{
		byID:      map[string]store.Role{"r1": storeRole("r1", "org-1", "Owner", true, true)},
		deleteErr: store.ErrImmutableBuiltin,
	}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodDelete, "/roles/r1", "", hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestDeleteRoleCustom(t *testing.T) {
	rs := &fakeRoleStore{byID: map[string]store.Role{
		"r2": storeRole("r2", "org-1", "Custom", false, false),
	}}
	h, hdr := rolesHarness(t, rs)

	rec := doJSON(t, h, http.MethodDelete, "/roles/r2", "", hdr)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if rs.gotDeleteID != "r2" {
		t.Errorf("deleted id = %q, want r2", rs.gotDeleteID)
	}
}

func TestRolesRequireRolesManage(t *testing.T) {
	rs := &fakeRoleStore{}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setRoleStore(t, rs)
	setResolver(t, permResolver(store.PermAgentsManage)) // not roles.manage
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodGet, "/roles", "", hdr)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// ---- members --------------------------------------------------------------

func membersHarness(t *testing.T, md memberDirectory, mrs memberRoleStore, rs roleStore) (http.Handler, http.Header) {
	t.Helper()
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setMemberDirectory(t, md)
	setMemberRoleStore(t, mrs)
	if rs != nil {
		setRoleStore(t, rs)
	}
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}
	return h, hdr
}

func TestListMembers(t *testing.T) {
	md := &fakeMemberDirectory{members: []store.Member{
		{ID: "m1", OrgID: "org-1", Email: "a@x", Active: true},
		{ID: "m2", OrgID: "org-1", Email: "b@x", Active: true},
	}}
	mrs := &fakeMemberRoleStore{rolesByMember: map[string][]store.Role{
		"m1": {storeRole("r1", "org-1", "Owner", true, true, store.PermRolesManage)},
	}}
	h, hdr := membersHarness(t, md, mrs, nil)

	rec := doJSON(t, h, http.MethodGet, "/members", "", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body apiMemberList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	if len(body.Items[0].Roles) != 1 || body.Items[0].Roles[0].Name != "Owner" {
		t.Errorf("m1 roles = %+v, want [Owner]", body.Items[0].Roles)
	}
	if body.Items[1].Roles == nil {
		t.Error("m2 roles serialised as null, want []")
	}
}

func TestUpdateMemberRolesReplace(t *testing.T) {
	md := &fakeMemberDirectory{byID: map[string]store.Member{
		"m1": {ID: "m1", OrgID: "org-1", Email: "a@x", Active: true},
	}}
	// Currently holds r1; desired is {r2} → remove r1, add r2.
	mrs := &fakeMemberRoleStore{rolesByMember: map[string][]store.Role{
		"m1": {storeRole("r1", "org-1", "Old", false, false)},
	}}
	rs := &fakeRoleStore{byID: map[string]store.Role{
		"r2": storeRole("r2", "org-1", "New", false, false),
	}}
	h, hdr := membersHarness(t, md, mrs, rs)

	rec := doJSON(t, h, http.MethodPut, "/members/m1/roles", `{"role_ids":["r2"]}`, hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(mrs.gotRemoved) != 1 || mrs.gotRemoved[0].role != "r1" {
		t.Errorf("removed = %v, want [r1]", mrs.gotRemoved)
	}
	if len(mrs.gotAssigned) != 1 || mrs.gotAssigned[0].role != "r2" {
		t.Errorf("assigned = %v, want [r2]", mrs.gotAssigned)
	}
}

func TestUpdateMemberRolesCrossOrgRole(t *testing.T) {
	md := &fakeMemberDirectory{byID: map[string]store.Member{
		"m1": {ID: "m1", OrgID: "org-1", Email: "a@x", Active: true},
	}}
	mrs := &fakeMemberRoleStore{}
	rs := &fakeRoleStore{byID: map[string]store.Role{
		"r2": storeRole("r2", "org-2", "Foreign", false, false),
	}}
	h, hdr := membersHarness(t, md, mrs, rs)

	rec := doJSON(t, h, http.MethodPut, "/members/m1/roles", `{"role_ids":["r2"]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if len(mrs.gotAssigned) != 0 || len(mrs.gotRemoved) != 0 {
		t.Error("assignments mutated despite a cross-org role")
	}
}

func TestUpdateMemberRolesLastOwner(t *testing.T) {
	md := &fakeMemberDirectory{byID: map[string]store.Member{
		"m1": {ID: "m1", OrgID: "org-1", Email: "a@x", Active: true},
	}}
	// Holds the Owner role; desired is empty → RemoveRole hits the last-owner
	// guard and the whole request is a 409.
	mrs := &fakeMemberRoleStore{
		rolesByMember: map[string][]store.Role{
			"m1": {storeRole("owner", "org-1", "Owner", true, true)},
		},
		removeErr: store.ErrLastOwner,
	}
	h, hdr := membersHarness(t, md, mrs, &fakeRoleStore{})

	rec := doJSON(t, h, http.MethodPut, "/members/m1/roles", `{"role_ids":[]}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateMemberRolesCrossOrgMember(t *testing.T) {
	md := &fakeMemberDirectory{byID: map[string]store.Member{
		"m1": {ID: "m1", OrgID: "org-2", Email: "a@x", Active: true},
	}}
	h, hdr := membersHarness(t, md, &fakeMemberRoleStore{}, &fakeRoleStore{})

	rec := doJSON(t, h, http.MethodPut, "/members/m1/roles", `{"role_ids":[]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}
