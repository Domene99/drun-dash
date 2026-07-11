package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockControlPlane returns an httptest server answering every dashboard and
// governance-page endpoint with canned data. When status is not 200 every
// endpoint responds with that status and a flat Error body, simulating a token
// the control plane rejects everywhere (not just on /auth/whoami).
func mockControlPlane(t *testing.T, status int) *httptest.Server {
	t.Helper()

	// rejected writes the error response and reports true when status models a
	// rejected token, so each handler can bail out uniformly.
	rejected := func(w http.ResponseWriter) bool {
		if status == http.StatusOK {
			return false
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"authentication required"}`))
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		if rejected(w) {
			return
		}
		_, _ = w.Write([]byte(`{"subject":"user-1","organization_id":"org-1","roles":["admin"]}`))
	})
	mux.HandleFunc("/agents", func(w http.ResponseWriter, r *http.Request) {
		if rejected(w) {
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"a1","name":"Planner","description":"plans things","latest_version":2,"created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		if rejected(w) {
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"session_id":"s1","agent_id":"a1","status":"running","fan_out_ceiling":4,"created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("/analytics/spend", func(w http.ResponseWriter, r *http.Request) {
		if rejected(w) {
			return
		}
		_, _ = w.Write([]byte(`{"from":"F","to":"T","currency":"USD","total":12.5,"groups":[{"key":"a1","amount":12.5,"tokens":100}]}`))
	})
	mux.HandleFunc("/analytics/budgets", func(w http.ResponseWriter, r *http.Request) {
		if rejected(w) {
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"b1","name":"Monthly cap","limit":100,"spent":80,"currency":"USD","period":"monthly"}]}`))
	})
	mux.HandleFunc("/approvals", func(w http.ResponseWriter, r *http.Request) {
		if rejected(w) {
			return
		}
		_, _ = w.Write([]byte(`{"items":[` +
			`{"id":"ap1","session_id":"s1","action":"spawn subagent","reason":"exceeds fan-out","status":"pending","created_at":"2026-07-01T00:00:00Z"},` +
			`{"id":"ap2","session_id":"s2","action":"external call","status":"approved","decided_by":"user-9","created_at":"2026-07-01T00:00:00Z"}` +
			`]}`))
	})
	mux.HandleFunc("/approvals/", func(w http.ResponseWriter, r *http.Request) {
		if rejected(w) {
			return
		}
		var body struct {
			Decision string `json:"decision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st := "approved"
		if body.Decision == "deny" {
			st = "denied"
		}
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"id":"ap1","session_id":"s1","action":"spawn subagent","status":%q,"decided_by":"user-1","created_at":"2026-07-01T00:00:00Z"}`, st)))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newServer builds an admin-ui Server whose client targets base.
func newServer(t *testing.T, base string) *Server {
	t.Helper()
	return New(NewClient(base, nil), nil)
}

// authedGet issues a GET to path carrying a valid session cookie.
func authedGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "good-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	// Every authenticated page must bounce an unauthenticated request to login.
	for _, path := range []string{"/", "/sessions", "/spend", "/approvals"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)

		if rec.Code < 300 || rec.Code >= 400 {
			t.Fatalf("%s status = %d, want a redirect", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("%s Location = %q, want /login", path, loc)
		}
	}
}

func TestLoginPageRenders(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="token"`) {
		t.Errorf("login page missing token field:\n%s", body)
	}
}

func TestLoginSetsCookieAndRedirects(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	form := strings.NewReader("token=abc.def")
	req := httptest.NewRequest(http.MethodPost, "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatalf("no %s cookie set; cookies = %v", sessionCookieName, cookies)
	}
	if found.Value != "abc.def" {
		t.Errorf("cookie value = %q, want abc.def", found.Value)
	}
	if !found.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
}

func TestLoginEmptyTokenRejected(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token=+"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" && c.MaxAge >= 0 {
			t.Errorf("empty-token login set a session cookie: %v", c)
		}
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "abc"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear the session cookie")
	}
}

func TestHealthz(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("healthz body = %q", rec.Body.String())
	}
}

func TestDashboardRendersWithCookie(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"user-1", "org-1", "Planner", "s1", "12.50 USD", "Live overview"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestDashboardUnauthorizedRedirects(t *testing.T) {
	cp := mockControlPlane(t, http.StatusUnauthorized)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("stale token was not cleared on 401")
	}
}

func TestSummaryFragmentRenders(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/fragments/summary")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="summary"`) || !strings.Contains(body, "Planner") {
		t.Errorf("summary fragment unexpected:\n%s", body)
	}
}

func TestSummaryFragmentUnauthorizedUsesHXRedirect(t *testing.T) {
	cp := mockControlPlane(t, http.StatusUnauthorized)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodGet, "/fragments/summary", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/login" {
		t.Errorf("HX-Redirect = %q, want /login", got)
	}
}

func TestSessionsPageRenders(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/sessions")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Sessions", "Fan-out", "s1", "a1", "running", ">4<"} {
		if !strings.Contains(body, want) {
			t.Errorf("sessions page missing %q:\n%s", want, body)
		}
	}
}

func TestSpendPageRenders(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/spend")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// spend total + group, plus budgets utilization (80/100 = 80.0%).
	for _, want := range []string{"12.50 USD", "Budgets", "Monthly cap", "80.00 USD", "100.00 USD", "80.0%"} {
		if !strings.Contains(body, want) {
			t.Errorf("spend page missing %q:\n%s", want, body)
		}
	}
}

func TestApprovalsPageRenders(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/approvals")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Pending item ap1 offers the decision action; decided item ap2 is read-only.
	for _, want := range []string{"Approvals", "ap1", "spawn subagent", "Approve", "Deny", `hx-post="/approvals/ap1/decision"`, "user-9"} {
		if !strings.Contains(body, want) {
			t.Errorf("approvals page missing %q:\n%s", want, body)
		}
	}
	// The already-approved ap2 must not offer a decision action.
	if strings.Contains(body, `hx-post="/approvals/ap2/decision"`) {
		t.Errorf("decided approval ap2 should be read-only:\n%s", body)
	}
}

func TestApprovalDecisionApprovePostsAndRerenders(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodPost, "/approvals/ap1/decision", strings.NewReader("decision=approve"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "good-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The re-rendered row reflects the control-plane decision and drops the
	// approve/deny buttons.
	if !strings.Contains(body, "approved") || !strings.Contains(body, "user-1") {
		t.Errorf("decision row did not reflect the result:\n%s", body)
	}
	if strings.Contains(body, "hx-post") {
		t.Errorf("decided row should no longer offer an action:\n%s", body)
	}
}

func TestApprovalDecisionInvalid(t *testing.T) {
	cp := mockControlPlane(t, http.StatusOK)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodPost, "/approvals/ap1/decision", strings.NewReader("decision=maybe"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "good-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid decision") {
		t.Errorf("expected an invalid-decision message:\n%s", rec.Body.String())
	}
}

func TestApprovalDecisionUnauthorizedUsesHXRedirect(t *testing.T) {
	cp := mockControlPlane(t, http.StatusUnauthorized)
	s := newServer(t, cp.URL)

	req := httptest.NewRequest(http.MethodPost, "/approvals/ap1/decision", strings.NewReader("decision=approve"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "stale"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/login" {
		t.Errorf("HX-Redirect = %q, want /login", got)
	}
}

func TestSpendPageSurfacesErrorReadably(t *testing.T) {
	// A control-plane 500 must render a readable message, not a blank page.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"backend down"}`))
	}))
	t.Cleanup(cp.Close)
	s := newServer(t, cp.URL)

	rec := authedGet(t, s, "/spend")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Could not load spend") || !strings.Contains(body, "backend down") {
		t.Errorf("spend error not surfaced readably:\n%s", body)
	}
	if !strings.Contains(body, "Could not load budgets") {
		t.Errorf("budgets error not surfaced readably:\n%s", body)
	}
}
