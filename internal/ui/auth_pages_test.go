package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// These tests drive the local-auth onboarding pages (signup, accept-invite),
// the primary password login, the Members-page invite action, and the new-agent
// form through the handlers against an in-memory mock control plane (the same
// socket-free roundtripper newServerCP uses), so they run even where binding a
// port is denied (the drun sandbox).

// unauthedPostForm issues a form POST without a session cookie, for the public
// auth endpoints.
func unauthedPostForm(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// --- Signup ----------------------------------------------------------------

func TestSignupPageRenders(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodGet, "/signup", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Create your organization",
		`name="org_name"`, `name="org_slug"`, `name="email"`, `name="name"`,
		`name="password"`, `name="password_confirm"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("signup page missing %q", want)
		}
	}
}

func TestSignupPasswordMismatchRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/signup", func(http.ResponseWriter, *http.Request) {
		t.Errorf("signup must not call the control plane on a client-side validation error")
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("org_name", "Acme")
	form.Set("email", "owner@acme.com")
	form.Set("password", "supersecret")
	form.Set("password_confirm", "different!")
	rec := unauthedPostForm(t, s, "/signup", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "do not match") {
		t.Errorf("expected a password-mismatch message:\n%s", rec.Body.String())
	}
}

func TestSignupShortPasswordRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/signup", func(http.ResponseWriter, *http.Request) {
		t.Errorf("signup must not call the control plane on a too-short password")
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("org_name", "Acme")
	form.Set("email", "owner@acme.com")
	form.Set("password", "short")
	form.Set("password_confirm", "short")
	rec := unauthedPostForm(t, s, "/signup", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least 8 characters") {
		t.Errorf("expected a min-length message:\n%s", rec.Body.String())
	}
}

func TestSignupSuccessSetsCookieAndRedirects(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/signup", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sess-abc","token_type":"Bearer","expires_in":3600}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("org_name", "Acme, Inc.")
	form.Set("org_slug", "acme")
	form.Set("email", "owner@acme.com")
	form.Set("name", "Ada")
	form.Set("password", "supersecret")
	form.Set("password_confirm", "supersecret")
	rec := unauthedPostForm(t, s, "/signup", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	c := cookieByName(rec.Result().Cookies(), sessionCookieName)
	if c == nil || c.Value != "sess-abc" || !c.HttpOnly {
		t.Fatalf("session cookie = %v, want value sess-abc (HttpOnly)", c)
	}
	var got SignupRequest
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode signup body: %v (%s)", err, gotBody)
	}
	if got.OrgName != "Acme, Inc." || got.OrgSlug != "acme" || got.Email != "owner@acme.com" || got.Name != "Ada" || got.Password != "supersecret" {
		t.Errorf("signup body = %+v", got)
	}
}

func TestSignupDuplicateShowsReadableMessage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/signup", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"conflict","message":"resource already exists"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("org_name", "Acme")
	form.Set("email", "owner@acme.com")
	form.Set("password", "supersecret")
	form.Set("password_confirm", "supersecret")
	rec := unauthedPostForm(t, s, "/signup", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered form)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "already in use") {
		t.Errorf("expected a duplicate slug/email message:\n%s", body)
	}
	if !strings.Contains(body, `value="Acme"`) {
		t.Errorf("submitted org name should be echoed back")
	}
}

// --- Password login --------------------------------------------------------

func TestPasswordLoginSuccess(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"access_token":"sess-xyz","token_type":"Bearer","expires_in":3600}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("org_slug", "acme")
	form.Set("email", "owner@acme.com")
	form.Set("password", "supersecret")
	rec := unauthedPostForm(t, s, "/login", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	if c := cookieByName(rec.Result().Cookies(), sessionCookieName); c == nil || c.Value != "sess-xyz" {
		t.Fatalf("session cookie = %v, want value sess-xyz", c)
	}
	var got map[string]string
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode login body: %v (%s)", err, gotBody)
	}
	if got["org_slug"] != "acme" || got["email"] != "owner@acme.com" || got["password"] != "supersecret" {
		t.Errorf("login body = %v", got)
	}
}

func TestPasswordLoginInvalidCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"invalid credentials"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("org_slug", "acme")
	form.Set("email", "owner@acme.com")
	form.Set("password", "wrongpass")
	rec := unauthedPostForm(t, s, "/login", form)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Invalid organization, email, or password") {
		t.Errorf("expected an invalid-credentials message:\n%s", body)
	}
	if !strings.Contains(body, `value="acme"`) {
		t.Errorf("submitted org slug should be echoed back")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" && c.MaxAge >= 0 {
			t.Errorf("invalid login must not set a session cookie: %v", c)
		}
	}
}

func TestPasswordLoginMissingFields(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", func(http.ResponseWriter, *http.Request) {
		t.Errorf("login must not call the control plane when a field is missing")
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("org_slug", "acme") // email and password omitted
	rec := unauthedPostForm(t, s, "/login", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// --- Accept invite ---------------------------------------------------------

func TestAcceptInvitePageCarriesToken(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodGet, "/accept-invite?token=inv-123", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="inv-123"`) {
		t.Errorf("invite token not carried into the hidden field:\n%s", body)
	}
	if !strings.Contains(body, `name="password"`) {
		t.Errorf("accept page missing password field")
	}
}

func TestAcceptInviteMissingTokenNotice(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	req := httptest.NewRequest(http.MethodGet, "/accept-invite", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "Missing invite token") {
		t.Errorf("expected a missing-token notice when no token is present")
	}
}

func TestAcceptInviteSuccess(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/accept-invite", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"access_token":"sess-inv","token_type":"Bearer","expires_in":3600}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("token", "inv-123")
	form.Set("name", "Grace")
	form.Set("password", "supersecret")
	form.Set("password_confirm", "supersecret")
	rec := unauthedPostForm(t, s, "/accept-invite", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if c := cookieByName(rec.Result().Cookies(), sessionCookieName); c == nil || c.Value != "sess-inv" {
		t.Fatalf("session cookie = %v, want value sess-inv", c)
	}
	var got map[string]string
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode accept body: %v (%s)", err, gotBody)
	}
	if got["token"] != "inv-123" || got["name"] != "Grace" || got["password"] != "supersecret" {
		t.Errorf("accept body = %v", got)
	}
}

func TestAcceptInviteInvalidToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/accept-invite", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","message":"invalid"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("token", "bad")
	form.Set("password", "supersecret")
	form.Set("password_confirm", "supersecret")
	rec := unauthedPostForm(t, s, "/accept-invite", form)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid, expired, or already used") {
		t.Errorf("expected an invalid-invite message")
	}
}

// --- Invite member ---------------------------------------------------------

func TestMemberInvitePostsAndSurfacesLink(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("members.manage"))
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[` +
			`{"id":"m1","email":"owner@acme.com","display_name":"Ada","active":true,"roles":[]},` +
			`{"id":"m2","email":"invitee@acme.com","active":false,"roles":[]}` +
			`]}`))
	})
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rolesJSON())
	})
	mux.HandleFunc("POST /members/invite", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"member":{"id":"m2","email":"invitee@acme.com","active":false,"roles":[]},` +
			`"invite_token":"raw-tok","invite_link":"http://admin.local/accept-invite?token=raw-tok","expires_at":"2026-07-22T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("email", "invitee@acme.com")
	form.Add("role_ids", "r-deploy")
	rec := authedPostForm(t, s, "/members/invite", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Invitation created") {
		t.Errorf("expected the invitation-created success alert")
	}
	if !strings.Contains(body, "http://admin.local/accept-invite?token=raw-tok") {
		t.Errorf("invite link not surfaced on the page:\n%s", body)
	}
	if !strings.Contains(body, "invitee@acme.com") || !strings.Contains(body, "Inactive") {
		t.Errorf("the pending (inactive) member should appear in the roster")
	}
	var got map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode invite body: %v (%s)", err, gotBody)
	}
	if got["email"] != "invitee@acme.com" {
		t.Errorf("invite email = %v", got["email"])
	}
	roles, _ := got["role_ids"].([]any)
	if len(roles) != 1 || roles[0] != "r-deploy" {
		t.Errorf("invite role_ids = %v", got["role_ids"])
	}
}

func TestMembersPageShowsInviteActionForManager(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("members.manage"))
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rolesJSON())
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/members")
	body := rec.Body.String()
	if !strings.Contains(body, "Invite member") {
		t.Errorf("Invite-member action missing for a members manager")
	}
	if !strings.Contains(body, `data-uk-modal-open="member-invite"`) {
		t.Errorf("Invite-member button should open the invite dialog")
	}
}

func TestMembersPageHidesInviteActionForAuditor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/members")
	if strings.Contains(rec.Body.String(), "Invite member") {
		t.Errorf("Invite-member action must be hidden without members.manage")
	}
}

// --- New agent form + create -----------------------------------------------

func TestNewAgentPageShowsFormWithPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"p1","name":"Default","current_version":1,"created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents/new")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"New agent",
		`name="name"`, `name="system_prompt"`, `name="default_policy_id"`,
		`name="endpoint_id"`,
		"Default",              // policy option
		"pin a model endpoint", // the run-requirement note
		"Tools, network, and concurrency are governed by the selected policy.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("new-agent page missing %q", want)
		}
	}
	// The policy owns the boundary now — the form no longer collects tools/fan-out.
	for _, gone := range []string{`name="allowed_tools"`, `name="fan_out_ceiling"`} {
		if strings.Contains(body, gone) {
			t.Errorf("new-agent page must not collect %q", gone)
		}
	}
}

func TestNewAgentPageNoAccessWithoutPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents/new")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No access") {
		t.Errorf("expected a no-access notice without agents.manage")
	}
	if strings.Contains(body, `name="system_prompt"`) {
		t.Errorf("the form must be hidden without agents.manage")
	}
}

func TestAgentCreatePublishesVersionAndRedirects(t *testing.T) {
	var createBody, versionBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("POST /agents", func(w http.ResponseWriter, r *http.Request) {
		createBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ag-9","name":"Release Bot","created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("POST /agents/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		versionBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_id":"ag-9","version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Release Bot")
	form.Set("description", "Ships releases")
	form.Set("system_prompt", "You are a release bot.")
	form.Set("default_policy_id", "p1")
	rec := authedPostForm(t, s, "/agents", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/agents/ag-9" {
		t.Errorf("Location = %q, want /agents/ag-9", loc)
	}
	var gotCreate AgentCreate
	if err := json.Unmarshal(createBody, &gotCreate); err != nil {
		t.Fatalf("decode create body: %v (%s)", err, createBody)
	}
	if gotCreate.Name != "Release Bot" || gotCreate.Description != "Ships releases" || gotCreate.DefaultPolicyID != "p1" {
		t.Errorf("create body = %+v", gotCreate)
	}
	var gotVer AgentVersionCreate
	if err := json.Unmarshal(versionBody, &gotVer); err != nil {
		t.Fatalf("decode version body: %v (%s)", err, versionBody)
	}
	if gotVer.SystemPrompt != "You are a release bot." {
		t.Errorf("version system_prompt = %q", gotVer.SystemPrompt)
	}
	// The policy governs the boundary (ADR-0010): the version publishes with the
	// default fan-out and no tools.
	if gotVer.FanOutCeiling != 0 {
		t.Errorf("version fan_out_ceiling = %d, want 0 (policy governs it)", gotVer.FanOutCeiling)
	}
	if len(gotVer.AllowedTools) != 0 {
		t.Errorf("version allowed_tools = %v, want none (policy governs tools)", gotVer.AllowedTools)
	}
}

func TestAgentCreateRequiresSystemPrompt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("POST /agents", func(http.ResponseWriter, *http.Request) {
		t.Errorf("agent must not be created without a system prompt")
	})
	mux.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Release Bot")
	form.Set("system_prompt", "   ") // blank
	rec := authedPostForm(t, s, "/agents", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered form)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "system prompt is required") {
		t.Errorf("expected a system-prompt-required message")
	}
}

func TestAgentsListShowsNewAgentButtonForManager(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents")
	if !strings.Contains(rec.Body.String(), `href="/agents/new"`) {
		t.Errorf("New-agent button missing for an agents manager:\n%s", rec.Body.String())
	}
}

func TestAgentsListHidesNewAgentButtonWithoutPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents")
	if strings.Contains(rec.Body.String(), `href="/agents/new"`) {
		t.Errorf("New-agent button must be hidden without agents.manage")
	}
}

// --- Edit agent form + update ----------------------------------------------

func TestAgentEditPageShowsFormWithPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("GET /agents/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ag-1","name":"Planner","description":"plans","default_policy_id":"p1","created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"p1","name":"Prod","current_version":1,"created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("GET /model-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"e1","name":"Anthropic prod","provider":"anthropic","created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents/ag-1/edit")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Edit agent", `action="/agents/ag-1/edit"`, "Save changes",
		`name="name"`, "Planner", `name="system_prompt"`, `name="default_policy_id"`, `name="endpoint_id"`,
		"Prod", "Anthropic prod",
		"Tools, network, and concurrency are governed by the selected policy.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit-agent page missing %q", want)
		}
	}
	// The agent's default policy is preselected.
	if !strings.Contains(body, `value="p1" selected`) {
		t.Errorf("edit form must preselect the agent's default policy:\n%s", body)
	}
	// Tools/fan-out are governed by the policy, not collected here.
	for _, gone := range []string{`name="allowed_tools"`, `name="fan_out_ceiling"`} {
		if strings.Contains(body, gone) {
			t.Errorf("edit-agent page must not collect %q", gone)
		}
	}
}

func TestAgentEditPageNoAccessWithoutPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents/ag-1/edit")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No access") {
		t.Errorf("expected a no-access notice without agents.manage")
	}
}

func TestAgentEditUpdatesIdentityAndPublishesVersion(t *testing.T) {
	var patchBody, versionBody []byte
	patched, published := false, false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("PATCH /agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		patched = true
		patchBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"ag-1","name":"Planner v2","description":"replans","created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /policies/{id}/versions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"pv-1","policy_id":"p1","version":1,"digest":"d1","signing":{"algorithm":"ed25519","key_id":"k","signature":"s"},"created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("POST /agents/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		published = true
		versionBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_id":"ag-1","version":2,"created_at":"2026-07-02T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Planner v2")
	form.Set("description", "replans")
	form.Set("system_prompt", "You are Planner v2.")
	form.Set("default_policy_id", "p1")
	form.Set("endpoint_id", "e1")
	rec := authedPostForm(t, s, "/agents/ag-1/edit", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/agents/ag-1" {
		t.Errorf("Location = %q, want /agents/ag-1", loc)
	}
	if !patched {
		t.Errorf("expected a PATCH /agents/{id} to update identity")
	}
	var gotPatch AgentUpdate
	if err := json.Unmarshal(patchBody, &gotPatch); err != nil {
		t.Fatalf("decode patch body: %v (%s)", err, patchBody)
	}
	if gotPatch.Name == nil || *gotPatch.Name != "Planner v2" || gotPatch.Description == nil || *gotPatch.Description != "replans" {
		t.Errorf("patch body = %+v", gotPatch)
	}
	if !published {
		t.Fatalf("expected a new version to be published when a system prompt is supplied")
	}
	var gotVer AgentVersionCreate
	if err := json.Unmarshal(versionBody, &gotVer); err != nil {
		t.Fatalf("decode version body: %v (%s)", err, versionBody)
	}
	if gotVer.SystemPrompt != "You are Planner v2." {
		t.Errorf("version system_prompt = %q", gotVer.SystemPrompt)
	}
	if gotVer.EndpointID != "e1" || gotVer.PolicyVersionID != "pv-1" {
		t.Errorf("version pins endpoint=%q policy_version=%q, want e1/pv-1", gotVer.EndpointID, gotVer.PolicyVersionID)
	}
	// The policy governs the boundary: no tools, default fan-out.
	if len(gotVer.AllowedTools) != 0 || gotVer.FanOutCeiling != 0 {
		t.Errorf("version must publish with no tools and default fan-out; got tools=%v fanout=%d", gotVer.AllowedTools, gotVer.FanOutCeiling)
	}
}

func TestAgentEditRenameOnlySkipsVersionPublish(t *testing.T) {
	patched := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("PATCH /agents/{id}", func(w http.ResponseWriter, _ *http.Request) {
		patched = true
		_, _ = w.Write([]byte(`{"id":"ag-1","name":"Renamed","created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("POST /agents/{id}/versions", func(http.ResponseWriter, *http.Request) {
		t.Errorf("a blank system prompt must not publish a new version")
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Renamed")
	form.Set("system_prompt", "   ") // blank → no new version
	rec := authedPostForm(t, s, "/agents/ag-1/edit", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if !patched {
		t.Errorf("rename must still PATCH the agent identity")
	}
}

func TestAgentEditRequiresName(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("PATCH /agents/{id}", func(http.ResponseWriter, *http.Request) {
		t.Errorf("agent must not be updated without a name")
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "")
	rec := authedPostForm(t, s, "/agents/ag-1/edit", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered form)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "name is required") {
		t.Errorf("expected a name-required message")
	}
}

// --- Auth gating for the new authenticated routes --------------------------

func TestNewAuthedRoutesRedirectUnauthenticated(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/agents/new"},
		{http.MethodPost, "/agents"},
		{http.MethodGet, "/agents/ag-1/edit"},
		{http.MethodPost, "/agents/ag-1/edit"},
		{http.MethodPost, "/members/invite"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s %s status = %d, want 303", tc.method, tc.path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("%s %s Location = %q, want /login", tc.method, tc.path, loc)
		}
	}
}
