package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/oidc"
	"github.com/Domene99/drun-dash/internal/store"
)

const (
	testIssuer   = "drun-dash"
	testAudience = "drun-dash-api"
	testKID      = "test"
	testTTL      = 15 * time.Minute
)

// init installs benign fakes for the member/org seams /auth/whoami enriches
// from, mirroring authz_test.go's init for the resolver seam. Whoami now loads
// the member and org for a member token, and the unit tests run without a
// database, so an un-overridden seam would dereference the nil pool. The
// defaults resolve the fixed member-1/org-1 principal the token helpers mint;
// tests that need other data override the seams with setMemberDirectory /
// setOrgByIDReader.
func init() {
	newMemberDirectory = func(store.Querier) memberDirectory {
		return &fakeMemberDirectory{byID: map[string]store.Member{"member-1": testMember()}}
	}
	newOrgByIDReader = func(store.Querier) orgByIDReader {
		return fakeOrgReader{byID: map[string]store.Organization{"org-1": testOrg()}}
	}
}

// fakeOIDC is a stub oidc.Verifier: it returns a fixed identity (or error)
// regardless of the raw token, so the exchange handler can be tested without
// constructing real ID tokens.
type fakeOIDC struct {
	token *oidc.IDToken
	err   error
}

func (f fakeOIDC) Verify(context.Context, string) (*oidc.IDToken, error) {
	return f.token, f.err
}

// fakeMembers is a stub MemberReader returning a fixed member (or error).
type fakeMembers struct {
	member store.Member
	err    error
}

func (f fakeMembers) GetByIDPSubject(context.Context, string, string) (store.Member, error) {
	return f.member, f.err
}

// fakeOrgReader is a stub orgByIDReader returning a fixed org (or error) so
// /auth/whoami's org lookup can be exercised without a database.
type fakeOrgReader struct {
	byID map[string]store.Organization
	err  error
}

func (f fakeOrgReader) GetByID(_ context.Context, id string) (store.Organization, error) {
	if f.err != nil {
		return store.Organization{}, f.err
	}
	if o, ok := f.byID[id]; ok {
		return o, nil
	}
	return store.Organization{}, store.ErrNotFound
}

// setOrgByIDReader points the newOrgByIDReader seam at r for the duration of
// the test, restoring the previous factory afterwards. Because it mutates a
// package-level var, callers must not run in parallel.
func setOrgByIDReader(t *testing.T, r orgByIDReader) {
	t.Helper()
	orig := newOrgByIDReader
	newOrgByIDReader = func(store.Querier) orgByIDReader { return r }
	t.Cleanup(func() { newOrgByIDReader = orig })
}

// noRevoker is an auth.Revoker that never revokes; NewVerifier requires a
// non-nil revoker.
type noRevoker struct{}

func (noRevoker) Revoke(context.Context, string, time.Duration) error { return nil }
func (noRevoker) IsRevoked(context.Context, string) (bool, error)     { return false, nil }

// testDeps builds a Deps wired to a shared Ed25519 key so tokens minted by the
// issuer verify against the verifier, plus the supplied OIDC and member stubs.
func testDeps(t *testing.T, o oidc.Verifier, m MemberReader) Deps {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer, err := auth.NewIssuer(auth.IssuerConfig{
		SigningKey: priv,
		KID:        testKID,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        testTTL,
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{testKID: pub},
		Issuer:   testIssuer,
		Audience: testAudience,
		Revoker:  noRevoker{},
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return Deps{
		OIDC:      o,
		Issuer:    issuer,
		Verifier:  verifier,
		Members:   m,
		AccessTTL: testTTL,
	}
}

func testMember() store.Member {
	return store.Member{
		ID:         "member-1",
		OrgID:      "org-1",
		IDPSubject: "idp-sub-1",
		Email:      "admin@example.com",
		Role:       store.RoleAdmin,
		Active:     true,
	}
}

func testOrg() store.Organization {
	return store.Organization{
		ID:   "org-1",
		Slug: "acme",
		Name: "Acme, Inc.",
	}
}

func mountRouter(deps Deps) http.Handler {
	r := chi.NewRouter()
	Mount(r, deps)
	return r
}

func doJSON(t *testing.T, h http.Handler, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSessionExchangeAndWhoami is the end-to-end proof of the auth vertical: a
// valid OIDC token is exchanged for an access token, which then authenticates a
// call to the protected /auth/whoami and returns the expected identity,
// including the member and org resolved from the database.
func TestSessionExchangeAndWhoami(t *testing.T) {
	deps := testDeps(t,
		fakeOIDC{token: &oidc.IDToken{Subject: "idp-sub-1", Email: "admin@example.com"}},
		fakeMembers{member: testMember()},
	)
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodPost, "/auth/session",
		`{"id_token":"raw","organization_id":"org-1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("access_token is empty")
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", tok.TokenType)
	}
	if tok.ExpiresIn != int(testTTL.Seconds()) {
		t.Errorf("expires_in = %d, want %d", tok.ExpiresIn, int(testTTL.Seconds()))
	}

	whoami := doJSON(t, h, http.MethodGet, "/auth/whoami", "",
		http.Header{"Authorization": {"Bearer " + tok.AccessToken}})
	if whoami.Code != http.StatusOK {
		t.Fatalf("whoami status = %d, want 200 (body %s)", whoami.Code, whoami.Body.String())
	}
	var id identityResponse
	if err := json.Unmarshal(whoami.Body.Bytes(), &id); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if id.Subject != "idp-sub-1" {
		t.Errorf("subject = %q, want idp-sub-1", id.Subject)
	}
	if id.OrganizationID != "org-1" {
		t.Errorf("organization_id = %q, want org-1", id.OrganizationID)
	}
	if len(id.Roles) != 1 || id.Roles[0] != "admin" {
		t.Errorf("roles = %v, want [admin]", id.Roles)
	}
	// Member and org fields are resolved from the database (the whoami seams),
	// not echoed from the token.
	if id.Email != "admin@example.com" {
		t.Errorf("email = %q, want admin@example.com", id.Email)
	}
	if id.OrgName != "Acme, Inc." {
		t.Errorf("org_name = %q, want Acme, Inc.", id.OrgName)
	}
	if id.OrgSlug != "acme" {
		t.Errorf("org_slug = %q, want acme", id.OrgSlug)
	}
}

// TestWhoamiReturnsNameEmailAndOrg proves /auth/whoami resolves the signed-in
// member's display name and email and the org's name and slug from the database
// (the seams), so the admin UI can render the principal and tenant in plain text
// instead of the synthetic subject and the org UUID.
func TestWhoamiReturnsNameEmailAndOrg(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	name := "Ada Lovelace"
	setMemberDirectory(t, &fakeMemberDirectory{byID: map[string]store.Member{
		"member-1": {ID: "member-1", OrgID: "org-1", Email: "ada@acme.test", DisplayName: &name, Active: true},
	}})
	setOrgByIDReader(t, fakeOrgReader{byID: map[string]store.Organization{
		"org-1": {ID: "org-1", Slug: "acme", Name: "Acme, Inc."},
	}})
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
	if id.Name != "Ada Lovelace" {
		t.Errorf("name = %q, want Ada Lovelace", id.Name)
	}
	if id.Email != "ada@acme.test" {
		t.Errorf("email = %q, want ada@acme.test", id.Email)
	}
	if id.OrgName != "Acme, Inc." {
		t.Errorf("org_name = %q, want Acme, Inc.", id.OrgName)
	}
	if id.OrgSlug != "acme" {
		t.Errorf("org_slug = %q, want acme", id.OrgSlug)
	}
	// Subject and organization_id remain the token's raw values (back-compat).
	if id.Subject != "idp-sub-1" {
		t.Errorf("subject = %q, want idp-sub-1", id.Subject)
	}
	if id.OrganizationID != "org-1" {
		t.Errorf("organization_id = %q, want org-1", id.OrganizationID)
	}
}

// TestWhoamiMemberlessToken proves a roles-only token (no member id) still
// returns 200 with the echoed subject/org and no member/org lookup: the seams
// are wired to fail, yet the response is unaffected and carries empty
// name/email/org and permissions.
func TestWhoamiMemberlessToken(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{})
	// If the handler mistakenly looked these up for a member-less token, the
	// erroring seams would turn the response into a 500.
	setMemberDirectory(t, &fakeMemberDirectory{})
	setOrgByIDReader(t, fakeOrgReader{err: store.ErrNotFound})
	h := mountRouter(deps)

	tok, err := deps.Issuer.Mint(context.Background(), auth.Claims{
		OrgID:            "org-1",
		Roles:            []string{"admin"},
		RegisteredClaims: jwt.RegisteredClaims{Subject: "roles-only"},
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	rec := doJSON(t, h, http.MethodGet, "/auth/whoami", "",
		http.Header{"Authorization": {"Bearer " + tok}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var id identityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if id.Subject != "roles-only" {
		t.Errorf("subject = %q, want roles-only", id.Subject)
	}
	if id.OrganizationID != "org-1" {
		t.Errorf("organization_id = %q, want org-1", id.OrganizationID)
	}
	if id.Name != "" || id.Email != "" || id.OrgName != "" || id.OrgSlug != "" {
		t.Errorf("member/org fields = %q/%q/%q/%q, want all empty", id.Name, id.Email, id.OrgName, id.OrgSlug)
	}
	if len(id.Permissions) != 0 {
		t.Errorf("permissions = %v, want empty", id.Permissions)
	}
}

// TestWhoamiLookupErrorIs500 proves a member or org lookup failure for a member
// token is answered with a 500 (the generic internal-error body), matching the
// permission-error posture; the response never discloses which lookup failed.
func TestWhoamiLookupErrorIs500(t *testing.T) {
	tests := []struct {
		name string
		set  func(t *testing.T)
	}{
		{
			name: "member lookup fails",
			set: func(t *testing.T) {
				// Empty directory -> GetByID returns ErrNotFound for member-1.
				setMemberDirectory(t, &fakeMemberDirectory{})
			},
		},
		{
			name: "org lookup fails",
			set: func(t *testing.T) {
				// Member resolves, but the org lookup errors.
				setMemberDirectory(t, &fakeMemberDirectory{byID: map[string]store.Member{"member-1": testMember()}})
				setOrgByIDReader(t, fakeOrgReader{err: store.ErrNotFound})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := testDeps(t, fakeOIDC{}, fakeMembers{})
			tc.set(t)
			h := mountRouter(deps)
			rec := doJSON(t, h, http.MethodGet, "/auth/whoami", "",
				http.Header{"Authorization": {"Bearer " + agentToken(t, deps, "org-1")}})
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSessionExchangeValidation(t *testing.T) {
	deps := testDeps(t, fakeOIDC{token: &oidc.IDToken{Subject: "idp-sub-1"}}, fakeMembers{member: testMember()})
	h := mountRouter(deps)

	tests := []struct {
		name string
		body string
	}{
		{"missing id_token", `{"organization_id":"org-1"}`},
		{"missing organization_id", `{"id_token":"raw"}`},
		{"malformed json", `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/auth/session", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSessionExchangeUnauthorized(t *testing.T) {
	tests := []struct {
		name string
		deps Deps
	}{
		{
			name: "invalid id token",
			deps: func() Deps {
				return testDeps(t, fakeOIDC{err: oidc.ErrMissingSubject}, fakeMembers{member: testMember()})
			}(),
		},
		{
			name: "unknown subject",
			deps: func() Deps {
				return testDeps(t, fakeOIDC{token: &oidc.IDToken{Subject: "ghost"}}, fakeMembers{err: store.ErrNotFound})
			}(),
		},
		{
			name: "inactive member",
			deps: func() Deps {
				m := testMember()
				m.Active = false
				return testDeps(t, fakeOIDC{token: &oidc.IDToken{Subject: "idp-sub-1"}}, fakeMembers{member: m})
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := mountRouter(tc.deps)
			rec := doJSON(t, h, http.MethodPost, "/auth/session",
				`{"id_token":"raw","organization_id":"org-1"}`, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWhoamiRequiresAuth(t *testing.T) {
	deps := testDeps(t, fakeOIDC{}, fakeMembers{member: testMember()})
	h := mountRouter(deps)

	rec := doJSON(t, h, http.MethodGet, "/auth/whoami", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-token status = %d, want 401", rec.Code)
	}

	rec = doJSON(t, h, http.MethodGet, "/auth/whoami", "",
		http.Header{"Authorization": {"Bearer not-a-jwt"}})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad-token status = %d, want 401", rec.Code)
	}
}
