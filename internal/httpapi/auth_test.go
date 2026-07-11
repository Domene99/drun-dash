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
// call to the protected /auth/whoami and returns the expected identity.
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
