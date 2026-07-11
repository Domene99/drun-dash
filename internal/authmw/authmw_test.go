package authmw_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
)

// fakeRevoker is an in-memory [auth.Revoker] so the verifier can be constructed
// without Redis or any network. A revoked jti reports true; a non-nil err makes
// IsRevoked fail, exercising the verifier's fail-closed path.
type fakeRevoker struct {
	revoked map[string]bool
	err     error
}

func (f *fakeRevoker) Revoke(_ context.Context, jti string, _ time.Duration) error {
	f.revoked[jti] = true
	return nil
}

func (f *fakeRevoker) IsRevoked(_ context.Context, jti string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.revoked[jti], nil
}

var errRevocationDown = errors.New("revocation backend unavailable")

const (
	testKID      = "test-key-1"
	testIssuer   = "https://control-plane.drun-dash.test"
	testAudience = "drun-dash-control-plane"
	testTTL      = 15 * time.Minute
)

// harness bundles a matched issuer/verifier pair over a fresh keypair plus the
// shared fakeRevoker, so tests can mint tokens the verifier will accept and can
// flip revocation on and off. now, when non-nil, drives both clocks.
type harness struct {
	issuer   *auth.Issuer
	verifier *auth.Verifier
	revoker  *fakeRevoker
	pub      ed25519.PublicKey
}

func newHarness(t *testing.T, now func() time.Time) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	issuer, err := auth.NewIssuer(auth.IssuerConfig{
		SigningKey: priv,
		KID:        testKID,
		Issuer:     testIssuer,
		Audience:   testAudience,
		TTL:        testTTL,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	revoker := &fakeRevoker{revoked: map[string]bool{}}
	verifier, err := auth.NewVerifier(auth.VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{testKID: pub},
		Issuer:   testIssuer,
		Audience: testAudience,
		Revoker:  revoker,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return &harness{issuer: issuer, verifier: verifier, revoker: revoker, pub: pub}
}

// mint issues a token the harness verifier will accept.
func (h *harness) mint(t *testing.T) string {
	t.Helper()
	token, err := h.issuer.Mint(context.Background(), auth.Claims{
		OrgID:            "org-123",
		MemberID:         "member-abc",
		Roles:            []string{"member", "auditor"},
		RegisteredClaims: jwt.RegisteredClaims{Subject: "idp-subject-xyz"},
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return token
}

// principalHandler is the protected handler used in tests: it reads the
// principal via MustPrincipal, records it, and returns 200.
type principalHandler struct {
	called bool
	got    *auth.Claims
}

func (h *principalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	h.got = authmw.MustPrincipal(r.Context())
	w.WriteHeader(http.StatusOK)
}

func TestRequireAuth_ValidToken(t *testing.T) {
	h := newHarness(t, nil)
	handler := &principalHandler{}
	srv := authmw.RequireAuth(h.verifier)(handler)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+h.mint(t))
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !handler.called {
		t.Fatal("wrapped handler was not called on a valid token")
	}
	if handler.got == nil {
		t.Fatal("principal was nil in handler context")
	}
	if handler.got.OrgID != "org-123" {
		t.Errorf("principal OrgID = %q, want %q", handler.got.OrgID, "org-123")
	}
	if handler.got.MemberID != "member-abc" {
		t.Errorf("principal MemberID = %q, want %q", handler.got.MemberID, "member-abc")
	}
	if handler.got.Subject != "idp-subject-xyz" {
		t.Errorf("principal Subject = %q, want %q", handler.got.Subject, "idp-subject-xyz")
	}
}

// TestRequireAuth_ContextAccessors checks PrincipalFromContext sees the same
// claims a handler behind the middleware would read.
func TestRequireAuth_ContextAccessors(t *testing.T) {
	h := newHarness(t, nil)
	var fromCtx *auth.Claims
	var fromCtxOK bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromCtx, fromCtxOK = authmw.PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	srv := authmw.RequireAuth(h.verifier)(handler)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+h.mint(t))
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if !fromCtxOK {
		t.Fatal("PrincipalFromContext reported no principal behind RequireAuth")
	}
	if fromCtx == nil || fromCtx.MemberID != "member-abc" {
		t.Fatalf("PrincipalFromContext returned %+v, want MemberID member-abc", fromCtx)
	}
}

func TestRequireAuth_Rejects(t *testing.T) {
	// A clock fixed far in the future makes a freshly minted token read as
	// already expired, exercising the Verify-failure path without waiting.
	future := func() time.Time { return time.Unix(1<<33, 0) }

	tests := []struct {
		name string
		// build returns the verifier to protect with, the Authorization header
		// value, and whether to set the header at all.
		build func(t *testing.T) (v *auth.Verifier, header string, setHeader bool)
	}{
		{
			name: "absent header",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				return h.verifier, "", false
			},
		},
		{
			name: "empty header value",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				return h.verifier, "", true
			},
		},
		{
			name: "non-bearer scheme",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				return h.verifier, "Basic " + h.mint(t), true
			},
		},
		{
			name: "bearer without token",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				return h.verifier, "Bearer ", true
			},
		},
		{
			name: "malformed token",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				return h.verifier, "Bearer not-a-jwt", true
			},
		},
		{
			name: "expired token",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				token := h.mint(t)
				// Verify with a future clock so exp is in the past.
				v, err := auth.NewVerifier(auth.VerifierConfig{
					Keys:     map[string]ed25519.PublicKey{testKID: h.pub},
					Issuer:   testIssuer,
					Audience: testAudience,
					Revoker:  h.revoker,
					Now:      future,
				})
				if err != nil {
					t.Fatalf("NewVerifier(expiring): %v", err)
				}
				return v, "Bearer " + token, true
			},
		},
		{
			name: "wrong audience",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				token := h.mint(t)
				v, err := auth.NewVerifier(auth.VerifierConfig{
					Keys:     map[string]ed25519.PublicKey{testKID: h.pub},
					Issuer:   testIssuer,
					Audience: "some-other-audience",
					Revoker:  h.revoker,
				})
				if err != nil {
					t.Fatalf("NewVerifier(wrong aud): %v", err)
				}
				return v, "Bearer " + token, true
			},
		},
		{
			name: "bad signature (unknown kid)",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				signer := newHarness(t, nil)  // mints the token
				checker := newHarness(t, nil) // holds a different key under the same kid
				return checker.verifier, "Bearer " + signer.mint(t), true
			},
		},
		{
			name: "revoked token",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				token := h.mint(t)
				h.revoker.revoked[parseUnverified(t, token).ID] = true
				return h.verifier, "Bearer " + token, true
			},
		},
		{
			name: "revocation backend down (fail closed)",
			build: func(t *testing.T) (*auth.Verifier, string, bool) {
				h := newHarness(t, nil)
				token := h.mint(t)
				h.revoker.err = errRevocationDown
				return h.verifier, "Bearer " + token, true
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, header, setHeader := tc.build(t)
			handler := &principalHandler{}
			srv := authmw.RequireAuth(v)(handler)

			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if setHeader {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if handler.called {
				t.Errorf("wrapped handler ran on rejected request")
			}
			assertUnauthorized(t, rec)
		})
	}
}

// TestRequireAuth_CaseInsensitiveScheme confirms the Bearer scheme is matched
// case-insensitively per RFC 6750.
func TestRequireAuth_CaseInsensitiveScheme(t *testing.T) {
	h := newHarness(t, nil)
	handler := &principalHandler{}
	srv := authmw.RequireAuth(h.verifier)(handler)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "bEaReR "+h.mint(t))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !handler.called {
		t.Fatalf("case-insensitive Bearer rejected: status %d, called %v", rec.Code, handler.called)
	}
}

func TestMustPrincipal_PanicsWithoutMiddleware(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustPrincipal did not panic on a context without a principal")
		}
	}()
	authmw.MustPrincipal(context.Background())
}

func TestPrincipalFromContext_Absent(t *testing.T) {
	if claims, ok := authmw.PrincipalFromContext(context.Background()); ok || claims != nil {
		t.Fatalf("PrincipalFromContext(bare) = (%+v, %v), want (nil, false)", claims, ok)
	}
}

// assertUnauthorized checks the response is a flat 401 error envelope with the
// unauthorized code and a non-empty message.
func assertUnauthorized(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (raw: %s)", err, rec.Body.String())
	}
	if body.Code != httpx.CodeUnauthorized {
		t.Errorf("error code = %q, want %q", body.Code, httpx.CodeUnauthorized)
	}
	if body.Message == "" {
		t.Error("error message is empty")
	}
}

// parseUnverified extracts claims from a token without validating it, so a test
// can learn the jti it needs to revoke.
func parseUnverified(t *testing.T, token string) *auth.Claims {
	t.Helper()
	var claims auth.Claims
	if _, _, err := jwt.NewParser().ParseUnverified(token, &claims); err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	return &claims
}
