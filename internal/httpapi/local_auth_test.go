package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/store"
)

// ---- fakes ----------------------------------------------------------------

type fakeAccountStore struct {
	signupMember store.Member
	signupErr    error
	inviteMember store.Member
	inviteInv    store.Invitation
	inviteErr    error
	acceptMember store.Member
	acceptErr    error

	gotSignup store.SignupParams
	gotInvite store.InviteParams
	gotAccept store.AcceptInviteParams
}

func (f *fakeAccountStore) Signup(_ context.Context, p store.SignupParams) (store.Member, error) {
	f.gotSignup = p
	return f.signupMember, f.signupErr
}

func (f *fakeAccountStore) InviteMember(_ context.Context, p store.InviteParams) (store.Member, store.Invitation, error) {
	f.gotInvite = p
	if f.inviteErr != nil {
		return store.Member{}, store.Invitation{}, f.inviteErr
	}
	return f.inviteMember, f.inviteInv, nil
}

func (f *fakeAccountStore) AcceptInvite(_ context.Context, p store.AcceptInviteParams) (store.Member, error) {
	f.gotAccept = p
	return f.acceptMember, f.acceptErr
}

func setAccountStore(t *testing.T, s accountStore) {
	t.Helper()
	orig := newAccountStore
	newAccountStore = func(store.Querier) accountStore { return s }
	t.Cleanup(func() { newAccountStore = orig })
}

type fakeOrgResolver struct {
	org store.Organization
	err error
}

func (f fakeOrgResolver) GetBySlug(_ context.Context, _ string) (store.Organization, error) {
	return f.org, f.err
}

func setOrgResolver(t *testing.T, s orgResolver) {
	t.Helper()
	orig := newOrgResolver
	newOrgResolver = func(store.Querier) orgResolver { return s }
	t.Cleanup(func() { newOrgResolver = orig })
}

type fakeLoginMembers struct {
	member store.Member
	err    error
}

func (f fakeLoginMembers) GetByOrgEmail(_ context.Context, _, _ string) (store.Member, error) {
	return f.member, f.err
}

func setLoginMembers(t *testing.T, s loginMemberStore) {
	t.Helper()
	orig := newLoginMemberStore
	newLoginMemberStore = func(store.Querier) loginMemberStore { return s }
	t.Cleanup(func() { newLoginMemberStore = orig })
}

// ---- signup ---------------------------------------------------------------

func TestSignupSuccess(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	acc := &fakeAccountStore{signupMember: store.Member{
		ID: "m1", OrgID: "o1", IDPSubject: "local:abc", Role: store.RoleAdmin, Active: true,
	}}
	setAccountStore(t, acc)
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/signup",
		`{"org_name":"Acme Inc","email":"owner@example.com","name":"Owner","password":"supersecret"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tok.AccessToken == "" || tok.TokenType != "Bearer" {
		t.Fatalf("unexpected token response: %+v", tok)
	}
	// The slug was derived from the org name.
	if acc.gotSignup.OrgSlug != "acme-inc" {
		t.Errorf("derived slug = %q, want acme-inc", acc.gotSignup.OrgSlug)
	}
	// The password reached the store hashed, never in the clear.
	if acc.gotSignup.PasswordHash == "" || acc.gotSignup.PasswordHash == "supersecret" {
		t.Errorf("password hash = %q, want a bcrypt hash", acc.gotSignup.PasswordHash)
	}
	if acc.gotSignup.Email != "owner@example.com" {
		t.Errorf("email = %q", acc.gotSignup.Email)
	}
}

func TestSignupValidation(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{})
	h := mountRouter(deps)

	tests := []struct {
		name string
		body string
	}{
		{"missing org_name", `{"email":"o@e.com","password":"supersecret"}`},
		{"missing email", `{"org_name":"Acme","password":"supersecret"}`},
		{"short password", `{"org_name":"Acme","email":"o@e.com","password":"short"}`},
		{"unsluggable org_name", `{"org_name":"!!!","email":"o@e.com","password":"supersecret"}`},
		{"malformed json", `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/auth/signup", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSignupConflict(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{signupErr: store.ErrConflict})
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/signup",
		`{"org_name":"Acme","email":"o@e.com","password":"supersecret"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

// ---- login ----------------------------------------------------------------

func TestLoginSuccess(t *testing.T) {
	hash, err := hashPassword("supersecret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setOrgResolver(t, fakeOrgResolver{org: store.Organization{ID: "o1", Slug: "acme"}})
	setLoginMembers(t, fakeLoginMembers{member: store.Member{
		ID: "m1", OrgID: "o1", IDPSubject: "local:x", Role: store.RoleMember, Active: true, PasswordHash: &hash,
	}})
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/login",
		`{"org_slug":"acme","email":"a@example.com","password":"supersecret"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("empty access token")
	}
}

func TestLoginUnauthorized(t *testing.T) {
	goodHash, _ := hashPassword("supersecret")
	otherHash, _ := hashPassword("different-password")

	tests := []struct {
		name  string
		setup func(t *testing.T)
		body  string
	}{
		{
			name: "unknown org",
			setup: func(t *testing.T) {
				setOrgResolver(t, fakeOrgResolver{err: store.ErrNotFound})
				setLoginMembers(t, fakeLoginMembers{err: store.ErrNotFound})
			},
			body: `{"org_slug":"nope","email":"a@e.com","password":"supersecret"}`,
		},
		{
			name: "unknown email",
			setup: func(t *testing.T) {
				setOrgResolver(t, fakeOrgResolver{org: store.Organization{ID: "o1", Slug: "acme"}})
				setLoginMembers(t, fakeLoginMembers{err: store.ErrNotFound})
			},
			body: `{"org_slug":"acme","email":"ghost@e.com","password":"supersecret"}`,
		},
		{
			name: "wrong password",
			setup: func(t *testing.T) {
				setOrgResolver(t, fakeOrgResolver{org: store.Organization{ID: "o1", Slug: "acme"}})
				setLoginMembers(t, fakeLoginMembers{member: store.Member{ID: "m1", OrgID: "o1", IDPSubject: "local:x", Active: true, PasswordHash: &otherHash}})
			},
			body: `{"org_slug":"acme","email":"a@e.com","password":"supersecret"}`,
		},
		{
			name: "no password set",
			setup: func(t *testing.T) {
				setOrgResolver(t, fakeOrgResolver{org: store.Organization{ID: "o1", Slug: "acme"}})
				setLoginMembers(t, fakeLoginMembers{member: store.Member{ID: "m1", OrgID: "o1", IDPSubject: "local:x", Active: true, PasswordHash: nil}})
			},
			body: `{"org_slug":"acme","email":"a@e.com","password":"supersecret"}`,
		},
		{
			name: "inactive member",
			setup: func(t *testing.T) {
				setOrgResolver(t, fakeOrgResolver{org: store.Organization{ID: "o1", Slug: "acme"}})
				setLoginMembers(t, fakeLoginMembers{member: store.Member{ID: "m1", OrgID: "o1", IDPSubject: "local:x", Active: false, PasswordHash: &goodHash}})
			},
			body: `{"org_slug":"acme","email":"a@e.com","password":"supersecret"}`,
		},
		{
			name: "missing fields",
			setup: func(t *testing.T) {
				setOrgResolver(t, fakeOrgResolver{org: store.Organization{ID: "o1", Slug: "acme"}})
				setLoginMembers(t, fakeLoginMembers{err: store.ErrNotFound})
			},
			body: `{"org_slug":"acme"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			tc.setup(t)
			h := mountRouter(deps)
			rec := doJSON(t, h, http.MethodPost, "/auth/login", tc.body, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---- invite ---------------------------------------------------------------

func TestInviteMemberSuccess(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	deps.AdminUIBaseURL = "https://admin.example.com"
	acc := &fakeAccountStore{inviteMember: store.Member{ID: "m2", OrgID: "org-1", Email: "invitee@example.com", Active: false}}
	setAccountStore(t, acc)
	setRoleStore(t, &fakeRoleStore{byID: map[string]store.Role{"r1": {ID: "r1", OrgID: "org-1", Name: "Member"}}})
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/members/invite",
		`{"email":"invitee@example.com","role_ids":["r1"]}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var resp inviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.InviteToken == "" {
		t.Error("empty invite token")
	}
	if !strings.Contains(resp.InviteLink, "https://admin.example.com/accept-invite?token=") {
		t.Errorf("invite link = %q", resp.InviteLink)
	}
	if !resp.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %v, want a future time", resp.ExpiresAt)
	}
	// The store received the org, the validated role, and the token HASH (not the raw token).
	if acc.gotInvite.OrgID != "org-1" || len(acc.gotInvite.RoleIDs) != 1 || acc.gotInvite.RoleIDs[0] != "r1" {
		t.Errorf("invite params = %+v", acc.gotInvite)
	}
	if acc.gotInvite.TokenHash == "" || acc.gotInvite.TokenHash == resp.InviteToken {
		t.Errorf("token hash = %q, want the hash of the raw token", acc.gotInvite.TokenHash)
	}
	if acc.gotInvite.TokenHash != hashToken(resp.InviteToken) {
		t.Errorf("stored hash does not match hash of returned token")
	}
}

func TestInviteMemberUnknownRole(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{})
	setRoleStore(t, &fakeRoleStore{byID: map[string]store.Role{}}) // GetByID → ErrNotFound
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/members/invite",
		`{"email":"x@example.com","role_ids":["ghost"]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestInviteMemberCrossOrgRole(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{})
	setRoleStore(t, &fakeRoleStore{byID: map[string]store.Role{"r9": {ID: "r9", OrgID: "org-2", Name: "Member"}}})
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/members/invite",
		`{"email":"x@example.com","role_ids":["r9"]}`, hdr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestInviteMemberConflict(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{inviteErr: store.ErrConflict})
	setRoleStore(t, &fakeRoleStore{byID: map[string]store.Role{}})
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/members/invite",
		`{"email":"dupe@example.com","role_ids":[]}`, hdr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestInviteMemberValidation(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{})
	setRoleStore(t, &fakeRoleStore{byID: map[string]store.Role{}})
	h := mountRouter(deps)
	hdr := http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}}

	rec := doJSON(t, h, http.MethodPost, "/members/invite", `{"role_ids":[]}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// ---- accept-invite --------------------------------------------------------

func TestAcceptInviteSuccess(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	acc := &fakeAccountStore{acceptMember: store.Member{
		ID: "m2", OrgID: "o1", IDPSubject: "local:y", Role: store.RoleMember, Active: true,
	}}
	setAccountStore(t, acc)
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/accept-invite",
		`{"token":"raw-invite-token","name":"Bob","password":"supersecret"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("empty access token")
	}
	// The store received the token's HASH and a bcrypt password hash.
	if acc.gotAccept.TokenHash != hashToken("raw-invite-token") {
		t.Errorf("token hash = %q, want the hash of the raw token", acc.gotAccept.TokenHash)
	}
	if acc.gotAccept.PasswordHash == "" || acc.gotAccept.PasswordHash == "supersecret" {
		t.Errorf("password hash = %q, want a bcrypt hash", acc.gotAccept.PasswordHash)
	}
}

func TestAcceptInviteInvalid(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{acceptErr: store.ErrNotFound})
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/accept-invite",
		`{"token":"bad","name":"Bob","password":"supersecret"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAcceptInviteValidation(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	setAccountStore(t, &fakeAccountStore{})
	h := mountRouter(deps)

	tests := []struct {
		name string
		body string
	}{
		{"missing token", `{"password":"supersecret"}`},
		{"short password", `{"token":"raw","password":"short"}`},
		{"malformed json", `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/auth/accept-invite", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---- deriveSlug -----------------------------------------------------------

func TestDeriveSlug(t *testing.T) {
	cases := map[string]string{
		"Acme Inc":        "acme-inc",
		"  Spaced  Out  ": "spaced-out",
		"Café & Bar!":     "caf-bar",
		"already-slug":    "already-slug",
		"!!!":             "",
		"":                "",
	}
	for in, want := range cases {
		if got := deriveSlug(in); got != want {
			t.Errorf("deriveSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
