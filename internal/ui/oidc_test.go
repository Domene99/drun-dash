package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// mockIdP returns an httptest server that mimics the parts of an OIDC identity
// provider the admin UI touches: OIDC discovery (so go-oidc can learn the
// authorize/token endpoints) and the token endpoint (which returns an
// id_token). It does not verify PKCE or client credentials — the admin UI does
// not verify the ID token either; the control plane does, on /auth/session.
func mockIdP(t *testing.T) *httptest.Server {
	t.Helper()

	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{` +
			`"issuer":"` + issuer + `",` +
			`"authorization_endpoint":"` + issuer + `/authorize",` +
			`"token_endpoint":"` + issuer + `/token",` +
			`"jwks_uri":"` + issuer + `/jwks"` +
			`}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{` +
			`"access_token":"idp-access-token",` +
			`"token_type":"Bearer",` +
			`"expires_in":3600,` +
			`"id_token":"header.payload.signature"` +
			`}`))
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL
	return srv
}

// newOIDCServer wires an admin-ui Server whose OIDC authenticator points at idp
// and whose control-plane client points at cpURL.
func newOIDCServer(t *testing.T, idp *httptest.Server, cpURL string) *Server {
	t.Helper()
	authr, err := NewOIDCAuthenticator(context.Background(), OIDCConfig{
		Issuer:         idp.URL,
		ClientID:       "admin-ui",
		ClientSecret:   "s3cret",
		RedirectURL:    "https://admin.example/callback",
		OrganizationID: "org-1",
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	return New(NewClient(cpURL, nil), nil, WithOIDC(authr))
}

// cookieByName returns the last Set-Cookie with the given name, or nil.
func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == name {
			found = c
		}
	}
	return found
}

func TestLoginRedirectsToIdPWhenOIDCConfigured(t *testing.T) {
	idp := mockIdP(t)
	s := newOIDCServer(t, idp, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodGet, "/login/sso", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, idp.URL+"/authorize") {
		t.Fatalf("Location = %q, want authorize URL at the IdP", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := u.Query()
	if q.Get("state") == "" {
		t.Error("authorize URL missing state")
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Errorf("authorize URL missing PKCE S256 challenge: %v", q)
	}
	// The state and PKCE verifier must be persisted in transient cookies.
	cookies := rec.Result().Cookies()
	if c := cookieByName(cookies, oidcStateCookie); c == nil || c.Value == "" || !c.HttpOnly {
		t.Errorf("missing/insecure %s cookie: %v", oidcStateCookie, c)
	}
	if c := cookieByName(cookies, oidcVerifierCookie); c == nil || c.Value == "" || !c.HttpOnly {
		t.Errorf("missing/insecure %s cookie: %v", oidcVerifierCookie, c)
	}
}

func TestLoginDevFallbackWhenOIDCUnset(t *testing.T) {
	// Without WithOIDC the paste-token dev form must still render.
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="token"`) {
		t.Errorf("dev fallback login missing token field:\n%s", rec.Body.String())
	}
}

func TestOIDCCallbackExchangesAndSetsSessionCookie(t *testing.T) {
	idp := mockIdP(t)

	var sessionBody map[string]string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/session" || r.Method != http.MethodPost {
			t.Errorf("unexpected control-plane call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("/auth/session should be unauthenticated, got Authorization = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&sessionBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"session-jwt-xyz","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(cp.Close)

	s := newOIDCServer(t, idp, cp.URL)

	// Step 1: begin login to obtain a valid state + verifier cookie pair.
	loginRec := httptest.NewRecorder()
	s.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/login/sso", nil))
	stateCookie := cookieByName(loginRec.Result().Cookies(), oidcStateCookie)
	verifierCookie := cookieByName(loginRec.Result().Cookies(), oidcVerifierCookie)
	if stateCookie == nil || verifierCookie == nil {
		t.Fatalf("login did not set transient cookies")
	}

	// Step 2: the IdP redirects back to /callback with the code and matching state.
	cbReq := httptest.NewRequest(http.MethodGet, "/callback?code=auth-code&state="+url.QueryEscape(stateCookie.Value), nil)
	cbReq.AddCookie(stateCookie)
	cbReq.AddCookie(verifierCookie)
	cbRec := httptest.NewRecorder()
	s.ServeHTTP(cbRec, cbReq)

	if cbRec.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303; body=%s", cbRec.Code, cbRec.Body.String())
	}
	if loc := cbRec.Header().Get("Location"); loc != "/" {
		t.Errorf("callback Location = %q, want /", loc)
	}
	session := cookieByName(cbRec.Result().Cookies(), sessionCookieName)
	if session == nil || session.Value != "session-jwt-xyz" {
		t.Fatalf("session cookie = %v, want value session-jwt-xyz", session)
	}
	if !session.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	// The ID token from the IdP and the org must reach /auth/session.
	if sessionBody["id_token"] != "header.payload.signature" {
		t.Errorf("id_token forwarded = %q", sessionBody["id_token"])
	}
	if sessionBody["organization_id"] != "org-1" {
		t.Errorf("organization_id forwarded = %q, want org-1", sessionBody["organization_id"])
	}
	// The transient cookies must be cleared after the callback consumes them.
	if c := cookieByName(cbRec.Result().Cookies(), oidcStateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("state cookie not cleared: %v", c)
	}
}

func TestOIDCCallbackStateMismatchRendersError(t *testing.T) {
	idp := mockIdP(t)
	s := newOIDCServer(t, idp, "http://control-plane.invalid")

	loginRec := httptest.NewRecorder()
	s.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/login/sso", nil))
	stateCookie := cookieByName(loginRec.Result().Cookies(), oidcStateCookie)
	verifierCookie := cookieByName(loginRec.Result().Cookies(), oidcVerifierCookie)

	// A forged state that does not match the cookie must be rejected.
	cbReq := httptest.NewRequest(http.MethodGet, "/callback?code=auth-code&state=forged-state", nil)
	cbReq.AddCookie(stateCookie)
	cbReq.AddCookie(verifierCookie)
	cbRec := httptest.NewRecorder()
	s.ServeHTTP(cbRec, cbReq)

	if cbRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (login page with error)", cbRec.Code)
	}
	if !strings.Contains(cbRec.Body.String(), "state mismatch") {
		t.Errorf("expected a state-mismatch message:\n%s", cbRec.Body.String())
	}
	if c := cookieByName(cbRec.Result().Cookies(), sessionCookieName); c != nil && c.Value != "" {
		t.Errorf("state mismatch must not set a session cookie: %v", c)
	}
}

func TestDevicePageRendersUserCode(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/device?user_code=WDJB-MJHT")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Approve device login", "WDJB-MJHT", `action="/device"`, "Approve", "Deny",
		`name="user_code"`, `value="approve"`, `value="deny"`} {
		if !strings.Contains(body, want) {
			t.Errorf("device page missing %q:\n%s", want, body)
		}
	}
}

func TestDevicePageMissingUserCode(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/device")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Missing device code") {
		t.Errorf("expected a missing-code message:\n%s", body)
	}
	// No actionable approve/deny form without a code.
	if strings.Contains(body, `name="user_code"`) {
		t.Errorf("device page should not offer a form without a code:\n%s", body)
	}
}

func TestDeviceApproveRecordsDecision(t *testing.T) {
	var got struct{ userCode, decision, auth string }
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/device/approve" || r.Method != http.MethodPost {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got.auth = r.Header.Get("Authorization")
		var body struct {
			UserCode string `json:"user_code"`
			Decision string `json:"decision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got.userCode, got.decision = body.UserCode, body.Decision
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(cp.Close)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodPost, "/device", strings.NewReader("user_code=WDJB-MJHT&decision=approve"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "member-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got.auth != "Bearer member-token" {
		t.Errorf("Authorization = %q, want Bearer member-token", got.auth)
	}
	if got.userCode != "WDJB-MJHT" || got.decision != "approve" {
		t.Errorf("approve payload = %+v", got)
	}
	if !strings.Contains(rec.Body.String(), "approved") {
		t.Errorf("expected a success confirmation:\n%s", rec.Body.String())
	}
}

func TestDeviceApproveInvalidDecision(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodPost, "/device", strings.NewReader("user_code=WDJB-MJHT&decision=maybe"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "member-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid decision") {
		t.Errorf("expected an invalid-decision message:\n%s", rec.Body.String())
	}
}

func TestDeviceApproveUnauthorizedRedirects(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"authentication required"}`))
	}))
	t.Cleanup(cp.Close)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodPost, "/device", strings.NewReader("user_code=WDJB-MJHT&decision=approve"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestClientExchangeSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/session" || r.Method != http.MethodPost {
			t.Errorf("call = %s %s, want POST /auth/session", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("session exchange must be unauthenticated, got %q", got)
		}
		var body struct {
			IDToken        string `json:"id_token"`
			OrganizationID string `json:"organization_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.IDToken != "id-token-abc" || body.OrganizationID != "org-7" {
			t.Errorf("body = %+v", body)
		}
		_, _ = w.Write([]byte(`{"access_token":"minted-jwt","token_type":"Bearer","expires_in":900}`))
	})

	jwt, err := c.ExchangeSession(context.Background(), "id-token-abc", "org-7")
	if err != nil {
		t.Fatalf("ExchangeSession: %v", err)
	}
	if jwt != "minted-jwt" {
		t.Errorf("jwt = %q, want minted-jwt", jwt)
	}
}

func TestClientExchangeSessionNoToken(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":900}`))
	})

	if _, err := c.ExchangeSession(context.Background(), "id-token", "org"); err == nil {
		t.Fatal("expected an error when /auth/session returns no access_token")
	}
}

func TestClientApproveDevice(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/device/approve" || r.Method != http.MethodPost {
			t.Errorf("call = %s %s, want POST /auth/device/approve", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer member-tok" {
			t.Errorf("Authorization = %q, want Bearer member-tok", got)
		}
		var body struct {
			UserCode string `json:"user_code"`
			Decision string `json:"decision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.UserCode != "WDJB-MJHT" || body.Decision != "deny" {
			t.Errorf("body = %+v", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.ApproveDevice(context.Background(), "member-tok", "WDJB-MJHT", "deny"); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
}
