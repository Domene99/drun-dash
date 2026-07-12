package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/httpapi"
)

// nopRevoker is an auth.Revoker that never revokes; auth.NewVerifier requires a
// non-nil revoker.
type nopRevoker struct{}

func (nopRevoker) Revoke(context.Context, string, time.Duration) error { return nil }
func (nopRevoker) IsRevoked(context.Context, string) (bool, error)     { return false, nil }

// bootRouterWithoutOIDC builds the production router via newRouter with a fully
// valid auth stack (issuer + verifier over a shared key) but OIDC left
// unconfigured — Deps.OIDC == nil — mirroring a boot where CP_OIDC_ISSUER /
// CP_OIDC_CLIENT_ID are unset (or the issuer was unreachable). This is exactly
// what buildAuthDeps produces in that case, so the router under test is the one
// the binary would serve.
func bootRouterWithoutOIDC(t *testing.T) http.Handler {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer, err := auth.NewIssuer(auth.IssuerConfig{
		SigningKey: priv,
		KID:        "test",
		Issuer:     defaultJWTIssuer,
		Audience:   defaultJWTAudience,
		TTL:        time.Minute,
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierConfig{
		Keys:     map[string]ed25519.PublicKey{"test": pub},
		Issuer:   defaultJWTIssuer,
		Audience: defaultJWTAudience,
		Revoker:  nopRevoker{},
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	deps := &httpapi.Deps{
		OIDC:      nil, // OIDC unconfigured: the defect under test.
		Issuer:    issuer,
		Verifier:  verifier,
		AccessTTL: time.Minute,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newRouter(logger, deps)
}

// TestRouterServesAPIWithoutOIDC is the boot-wiring regression guard: with OIDC
// unconfigured the control-plane must still mount and serve the whole REST API.
// Previously buildAuthDeps returned nil and newRouter skipped httpapi.Mount, so
// every route but /healthz and /readyz answered 404. This asserts the fixed
// behaviour: protected routes are mounted (401 without a token, NOT 404) and
// only the OIDC session exchange degrades (to 501).
func TestRouterServesAPIWithoutOIDC(t *testing.T) {
	h := bootRouterWithoutOIDC(t)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	// A protected resource route must be mounted: without a Bearer token the
	// auth middleware answers 401. A 404 here would mean the API never mounted
	// — the exact boot-wiring defect this test guards against.
	if rec := do(http.MethodGet, "/agents", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /agents without token = %d, want 401 (route must be mounted, not 404)", rec.Code)
	}

	// /auth/whoami needs only the always-built Verifier, so it too is mounted:
	// 401 without a token, never 404.
	if rec := do(http.MethodGet, "/auth/whoami", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /auth/whoami without token = %d, want 401 (route must be mounted)", rec.Code)
	}

	// GET /me/agents (the entitled-agents extension) is likewise behind auth
	// and mounted: 401 without a token.
	if rec := do(http.MethodGet, "/me/agents", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /me/agents without token = %d, want 401 (route must be mounted)", rec.Code)
	}

	// The session exchange is the one route that needs OIDC. With it nil the
	// handler degrades to 501 (not_implemented) rather than panicking on a nil
	// verifier or 404ing.
	if rec := do(http.MethodPost, "/auth/session", `{"id_token":"x","organization_id":"org-1"}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("POST /auth/session without OIDC = %d, want 501", rec.Code)
	}

	// Liveness is unaffected.
	if rec := do(http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

// TestBuildOIDCVerifierUnreachableIssuer proves the set-but-unreachable case:
// when CP_OIDC_ISSUER points at an issuer that fails discovery, buildOIDCVerifier
// logs and returns nil instead of erroring, so buildAuthDeps continues and the
// boot never crashes. 127.0.0.1:1 refuses immediately, keeping the test offline
// and fast.
func TestBuildOIDCVerifierUnreachableIssuer(t *testing.T) {
	t.Setenv(envOIDCIssuer, "http://127.0.0.1:1/")
	t.Setenv(envOIDCClientID, "some-client")

	v := buildOIDCVerifier(context.Background(), discardLogger())
	if v != nil {
		t.Fatalf("verifier = %v, want nil (unreachable issuer must degrade, not build)", v)
	}
}

// TestBuildOIDCVerifierUnset proves the unset case returns nil (OIDC disabled)
// rather than attempting discovery.
func TestBuildOIDCVerifierUnset(t *testing.T) {
	t.Setenv(envOIDCIssuer, "")
	t.Setenv(envOIDCClientID, "")

	if v := buildOIDCVerifier(context.Background(), discardLogger()); v != nil {
		t.Fatalf("verifier = %v, want nil when OIDC is unconfigured", v)
	}
}
