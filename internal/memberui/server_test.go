package memberui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestServer builds a Server whose control-plane client is routed at a mock
// server running fn. The Server's own handlers are exercised with
// httptest.NewRecorder (no socket bind), while the mock control plane binds a
// port (so these run on a host, not in the network-sandboxed builder).
func newTestServer(t *testing.T, fn http.HandlerFunc) *Server {
	t.Helper()
	cp := httptest.NewServer(fn)
	t.Cleanup(cp.Close)
	client := NewClient(cp.URL, cp.Client())
	return New(client, nil)
}

// cookieValue returns the value the response set for the named cookie, or "".
func cookieValue(resp *http.Response, name string) string {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// cookieCleared reports whether the response expired the named cookie.
func cookieCleared(resp *http.Response, name string) bool {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %q", rr.Body.String())
	}
}

func TestRequireAuthRedirectsToLogin(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestLoginRendersUserCodeAndSetsDeviceCookie(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceAuthorization{
			DeviceCode:              "dev-code",
			UserCode:                "WXYZ-1234",
			VerificationURI:         "http://admin/device",
			VerificationURIComplete: "http://admin/device?user_code=WXYZ-1234",
			Interval:                5,
		})
	})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/login", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "WXYZ-1234") {
		t.Errorf("body missing user code: %q", body)
	}
	if !strings.Contains(body, `hx-get="/login/poll"`) {
		t.Errorf("body missing poller: %q", body)
	}
	if got := cookieValue(rr.Result(), deviceCookieName); got != "dev-code" {
		t.Errorf("device cookie = %q, want dev-code", got)
	}
}

func TestLoginRedirectsWhenAlreadySignedIn(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("control plane should not be called when already signed in")
	})
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "existing"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Errorf("status=%d loc=%q, want 303 /", rr.Code, rr.Header().Get("Location"))
	}
}

func TestLoginPollApprovedSetsSessionCookie(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "jwt-token", TokenType: "Bearer", ExpiresIn: 3600})
	})
	req := httptest.NewRequest(http.MethodGet, "/login/poll", nil)
	req.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "dev-code"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q, want /", got)
	}
	if got := cookieValue(rr.Result(), sessionCookieName); got != "jwt-token" {
		t.Errorf("session cookie = %q, want jwt-token", got)
	}
	if !cookieCleared(rr.Result(), deviceCookieName) {
		t.Error("device cookie should be cleared after approval")
	}
}

func TestLoginPollPendingKeepsPolling(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	})
	req := httptest.NewRequest(http.MethodGet, "/login/poll", nil)
	req.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "dev-code"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `hx-get="/login/poll"`) {
		t.Errorf("pending fragment should keep polling: %q", rr.Body.String())
	}
	if rr.Header().Get("HX-Redirect") != "" {
		t.Error("pending poll should not redirect")
	}
}

func TestLoginPollDeniedStopsAndClearsCookie(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	})
	req := httptest.NewRequest(http.MethodGet, "/login/poll", nil)
	req.AddCookie(&http.Cookie{Name: deviceCookieName, Value: "dev-code"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	body := rr.Body.String()
	if strings.Contains(body, `hx-get="/login/poll"`) {
		t.Errorf("denied fragment should stop polling: %q", body)
	}
	if !cookieCleared(rr.Result(), deviceCookieName) {
		t.Error("device cookie should be cleared after denial")
	}
}

func TestLoginPollNoDeviceCookie(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("control plane should not be called without a device cookie")
	})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/login/poll", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `hx-get="/login/poll"`) {
		t.Error("missing device cookie should not keep polling")
	}
}

func TestHomeListsEntitledAgents(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/agents" {
			t.Errorf("path = %q, want /me/agents", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(AgentList{Items: []Agent{{ID: "a1", Name: "Alpha", Description: "the alpha agent"}}})
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Alpha") || !strings.Contains(body, "Launch") {
		t.Errorf("home body missing agent/launch: %q", body)
	}
}

func TestHomeUnauthorizedRedirects(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Errorf("status=%d loc=%q, want 303 /login", rr.Code, rr.Header().Get("Location"))
	}
}

func TestLaunchStoresSessionAndHidesProxyToken(t *testing.T) {
	const secret = "dgp_super_secret"
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(SessionLaunch{
			SessionID:     "sess-1",
			ProxyBaseURL:  "http://proxy:8081",
			ProxyToken:    secret,
			FanOutCeiling: 4,
			ExpiresAt:     "2026-07-11T12:00:00Z",
		})
	})

	form := url.Values{"agent_id": {"agent-9"}}
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sess-1") || !strings.Contains(body, "http://proxy:8081") {
		t.Errorf("launch response missing session details: %q", body)
	}
	if strings.Contains(body, secret) {
		t.Errorf("launch response leaked proxy token: %q", body)
	}

	// The session persists and is shown on the home page, still without the token.
	if _, ok := s.sessions["sess-1"]; !ok {
		t.Fatal("session was not stored server-side")
	}
	homeReq := httptest.NewRequest(http.MethodGet, "/", nil)
	homeReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	homeRR := httptest.NewRecorder()
	s.ServeHTTP(homeRR, homeReq)
	if hb := homeRR.Body.String(); strings.Contains(hb, secret) {
		t.Errorf("home page leaked proxy token: %q", hb)
	}
}

func TestLaunchMissingAgentID(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("control plane should not be called without agent_id")
	})
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestTerminateRemovesSession(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Session{SessionID: "sess-1", Status: "terminated"})
	})
	// Seed a launched session directly.
	s.storeSession(&launchedSession{SessionID: "sess-1", AgentID: "agent-9", ProxyBaseURL: "http://proxy", proxyToken: "secret", FanOut: 4})

	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/terminate", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if _, ok := s.sessions["sess-1"]; ok {
		t.Error("session should have been dropped after terminate")
	}
	if !strings.Contains(rr.Body.String(), "No active sessions") {
		t.Errorf("expected empty sessions section: %q", rr.Body.String())
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {})
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "tok"})
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Errorf("status=%d loc=%q, want 303 /login", rr.Code, rr.Header().Get("Location"))
	}
	if !cookieCleared(rr.Result(), sessionCookieName) {
		t.Error("session cookie should be cleared on logout")
	}
}
