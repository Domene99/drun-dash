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

// These tests drive the RBAC governance pages and the read-only Agents/Policies
// detail views through the handlers against an in-memory mock control plane (the
// same socket-free roundtripper newServerCP uses), so they run even where
// binding a port is denied (the drun sandbox).

// whoamiHandler answers GET /auth/whoami with a fixed identity carrying perms as
// the caller's effective permission set, which the pages use to gate actions.
func whoamiHandler(perms ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"subject":         "user-1",
			"organization_id": "org-1",
			"email":           "admin@example.com",
			"roles":           []string{"admin"},
			"permissions":     perms,
		})
		_, _ = w.Write(body)
	}
}

// authedPostForm issues a form POST carrying a valid session cookie.
func authedPostForm(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "good-token"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func rolesJSON() string {
	return `{"items":[` +
		`{"id":"r-owner","name":"Owner","description":"Super admin","builtin":true,"all_agents":true,"permissions":["roles.manage","agents.run"],"agents":[],"created_at":"2026-07-01T00:00:00Z"},` +
		`{"id":"r-deploy","name":"Deployer","description":"Ships releases","builtin":false,"all_agents":false,"permissions":["agents.run"],"agents":[{"agent_id":"a1"}],"created_at":"2026-07-02T00:00:00Z"}` +
		`]}`
}

func TestRolesPageRendersAndGates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("roles.manage"))
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"a1","name":"Planner","created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rolesJSON())
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/roles")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Roles", "Owner", "Deployer", "Built-in", "Custom", "New role",
		"Planner",                    // agent option in the create form
		"agents.run", "roles.manage", // permission checklist
		`data-uk-modal-open="role-edit-r-deploy"`,   // custom role is editable
		`data-uk-modal-open="role-delete-r-deploy"`, // and deletable
	} {
		if !strings.Contains(body, want) {
			t.Errorf("roles page missing %q", want)
		}
	}
	// The built-in role must not offer edit/delete controls.
	if strings.Contains(body, `data-uk-modal-open="role-edit-r-owner"`) {
		t.Errorf("built-in role must be read-only (no edit control)")
	}
}

// TestRolesPageShowsBuiltinPermissionsReadOnly asserts an admin can see what a
// built-in role grants (via a read-only "View permissions" panel) even though it
// cannot be edited.
func TestRolesPageShowsBuiltinPermissionsReadOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("roles.manage"))
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"a1","name":"Planner","created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rolesJSON())
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/roles")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The built-in Owner role gets a read-only view of its permissions.
	for _, want := range []string{
		"View permissions",
		`data-uk-modal-open="role-view-r-owner"`,
		"Permissions — Owner",
		"roles.manage", "agents.run", // the verbs Owner grants, in the panel
		"disabled", // rendered read-only (non-editable) checkboxes
	} {
		if !strings.Contains(body, want) {
			t.Errorf("built-in read-only permissions view missing %q", want)
		}
	}
	// The built-in role is not editable.
	if strings.Contains(body, `data-uk-modal-open="role-edit-r-owner"`) {
		t.Errorf("built-in role must not be editable")
	}
}

func TestRolesPageViewOnlyWithoutPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/roles")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "View-only") {
		t.Errorf("expected a view-only notice for a caller without roles.manage:\n%s", body)
	}
	if strings.Contains(body, "New role") {
		t.Errorf("New-role action must be hidden without roles.manage")
	}
}

func TestRoleCreatePostsAndRedirects(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("roles.manage"))
	mux.HandleFunc("POST /roles", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"r9","name":"Deploy","builtin":false,"all_agents":true,"permissions":[],"agents":[],"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Deploy")
	form.Set("description", "Ships releases")
	form.Set("all_agents", "on")
	form.Add("permissions", "agents.run")
	form.Add("permissions", "roles.manage")
	form.Add("agents", "a1")
	rec := authedPostForm(t, s, "/roles", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/roles" {
		t.Errorf("Location = %q, want /roles", loc)
	}
	var got RoleCreate
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode captured body: %v (%s)", err, gotBody)
	}
	if got.Name != "Deploy" || !got.AllAgents {
		t.Errorf("create body = %+v", got)
	}
	if len(got.Permissions) != 2 || len(got.Agents) != 1 || got.Agents[0].AgentID != "a1" {
		t.Errorf("create perms/agents = %+v / %+v", got.Permissions, got.Agents)
	}
}

func TestRoleDeletePostsAndRedirects(t *testing.T) {
	var deleted bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("roles.manage"))
	mux.HandleFunc("DELETE /roles/{roleId}", func(w http.ResponseWriter, _ *http.Request) {
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	s := newServerCP(t, mux)

	rec := authedPostForm(t, s, "/roles/r-deploy/delete", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if !deleted {
		t.Errorf("delete was not forwarded to the control plane")
	}
}

func membersJSON() string {
	return `{"items":[` +
		`{"id":"m1","email":"dev@example.com","display_name":"Dev One","active":true,"roles":[{"id":"r-deploy","name":"Deployer","builtin":false,"all_agents":false,"permissions":[],"agents":[],"created_at":"2026-07-02T00:00:00Z"}]},` +
		`{"id":"m2","email":"aud@example.com","active":false,"roles":[]}` +
		`]}`
}

func TestMembersPageRendersAndGates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("members.manage"))
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, membersJSON())
	})
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rolesJSON())
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/members")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Members", "Dev One", "dev@example.com", "Deployer", "Active", "Inactive",
		"Assign roles", `data-uk-modal-open="member-roles-m1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("members page missing %q", want)
		}
	}
}

func TestMembersViewOnlyForAuditor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, membersJSON())
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/members")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Dev One") {
		t.Errorf("auditor should still see the roster")
	}
	if strings.Contains(body, "Assign roles") {
		t.Errorf("assign action must be hidden without members.manage")
	}
}

func TestMembersNoAccessNotice(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("approvals.decide"))
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/members")
	body := rec.Body.String()
	if !strings.Contains(body, "No access") {
		t.Errorf("expected a no-access notice without members.manage or audit.view:\n%s", body)
	}
}

func TestMemberRolesPostsDesiredSet(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("members.manage"))
	mux.HandleFunc("PUT /members/{memberId}/roles", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"m1","email":"dev@example.com","active":true,"roles":[]}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Add("role_ids", "r-deploy")
	form.Add("role_ids", "r-owner")
	rec := authedPostForm(t, s, "/members/m1/roles", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	var got struct {
		RoleIDs []string `json:"role_ids"`
	}
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, gotBody)
	}
	if len(got.RoleIDs) != 2 {
		t.Errorf("role_ids = %v, want two", got.RoleIDs)
	}
}

func TestAgentsListLinksToDetail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler())
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"a1","name":"Planner","description":"plans","latest_version":2,"created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents")
	body := rec.Body.String()
	for _, want := range []string{"Agents", "Planner", `href="/agents/a1"`, ">2<"} {
		if !strings.Contains(body, want) {
			t.Errorf("agents list missing %q", want)
		}
	}
}

func TestAgentDetailResolvesNamesAndAccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("roles.manage", "agents.manage"))
	mux.HandleFunc("GET /agents/{agentId}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"a1","name":"Planner","description":"plans things","default_policy_id":"p1","latest_version":2,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /agents/{agentId}/versions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"agent_id":"a1","version":2,"manifest_digest":"sha256:abcdef123456","notes":"latest","created_at":"2026-07-02T00:00:00Z"}]}`))
	})
	mux.HandleFunc("GET /agents/{agentId}/entitlements", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"principal_type":"user","principal_id":"u-1234abcd","max_fan_out":3}]}`))
	})
	mux.HandleFunc("GET /policies/{policyId}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"p1","name":"Production","current_version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rolesJSON())
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents/a1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Planner", "plans things", "Production", // policy resolved to name
		`href="/policies/p1"`,                          // and linked
		"latest", `data-uk-copy="sha256:abcdef123456"`, // version + digest chip
		"user", `data-uk-copy="u-1234abcd"`, // entitlement principal
		"Owner", "Deployer", // roles granting access (all_agents + explicit)
		"Back to agents", "Edit", // read-only affordances
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agent detail missing %q", want)
		}
	}
}

func TestAgentDetailNotFoundShowsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler())
	mux.HandleFunc("GET /agents/{agentId}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"resource not found"}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/agents/missing")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Could not load agent") {
		t.Errorf("expected an inline error for an unknown agent")
	}
}

func TestPolicyDetailShowsVersions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler())
	mux.HandleFunc("GET /policies/{policyId}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"p1","name":"Production","description":"prod boundary","current_version":3,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /policies/{policyId}/versions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"policy_id":"p1","version":3,"digest":"sha256:deadbeef99","boundary":{"network":"allowlist","rules":{"network_allowlist":["api.anthropic.com","example.com"]}},"signing":{"algorithm":"ed25519","key_id":"key-1"},"created_at":"2026-07-03T00:00:00Z"}]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/policies/p1")
	body := rec.Body.String()
	for _, want := range []string{"Production", "prod boundary", "ed25519", "key-1", `data-uk-copy="sha256:deadbeef99"`, "Back to policies", "api.anthropic.com, example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("policy detail missing %q", want)
		}
	}
}

func TestPoliciesListLinksToDetail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler())
	mux.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"p1","name":"Production","current_version":3,"created_at":"2026-07-01T00:00:00Z"}]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/policies")
	body := rec.Body.String()
	for _, want := range []string{"Policies", "Production", `href="/policies/p1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("policies list missing %q", want)
		}
	}
}

func TestGovernancePagesRedirectUnauthenticated(t *testing.T) {
	s := newServer(t, "http://control-plane.invalid")
	for _, path := range []string{"/roles", "/members", "/agents", "/agents/a1", "/policies", "/policies/p1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("%s Location = %q, want /login", path, loc)
		}
	}
}

// TestPolicyCreateSetsPermittedRoles asserts that creating a policy with roles
// selected posts them to PUT /policies/{id}/roles using the id the create
// returned (policy-centric access, ADR-0010).
func TestPolicyCreateSetsPermittedRoles(t *testing.T) {
	var gotPath string
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("policies.manage"))
	mux.HandleFunc("POST /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"p9","name":"Read-only","current_version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("PUT /policies/{policyId}/roles", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Read-only")
	form.Add("permitted_roles", "r-deploy")
	form.Add("permitted_roles", "r-owner")
	rec := authedPostForm(t, s, "/policies", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if gotPath != "/policies/p9/roles" {
		t.Errorf("permitted-roles PUT path = %q, want /policies/p9/roles", gotPath)
	}
	var got struct {
		RoleIDs []string `json:"role_ids"`
	}
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, gotBody)
	}
	if len(got.RoleIDs) != 2 {
		t.Errorf("role_ids = %v, want two", got.RoleIDs)
	}
}

// TestPolicyCreateSetsNetworkAllowlist asserts that the allowed-hosts textarea is
// parsed (newline/comma separated, blanks dropped) into the created boundary's
// rules.network_allowlist, so an admin can pre-authorize egress with no approval.
func TestPolicyCreateSetsNetworkAllowlist(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("policies.manage"))
	mux.HandleFunc("POST /policies", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"p9","name":"Egress","current_version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Egress")
	form.Set("network", "allowlist")
	form.Set("network_allowlist", "api.anthropic.com\n , example.com\n\n")
	rec := authedPostForm(t, s, "/policies", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	var got struct {
		Boundary struct {
			Rules map[string]any `json:"rules"`
		} `json:"boundary"`
	}
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, gotBody)
	}
	raw, ok := got.Boundary.Rules["network_allowlist"].([]any)
	if !ok {
		t.Fatalf("network_allowlist = %v, want a string array", got.Boundary.Rules["network_allowlist"])
	}
	hosts := make([]string, len(raw))
	for i, v := range raw {
		hosts[i], _ = v.(string)
	}
	if len(hosts) != 2 || hosts[0] != "api.anthropic.com" || hosts[1] != "example.com" {
		t.Errorf("network_allowlist = %v, want [api.anthropic.com example.com]", hosts)
	}
}

// TestPolicyCreateOmitsEmptyNetworkAllowlist asserts an empty allowed-hosts field
// leaves the boundary's rules unset (a clean boundary), not an empty array.
func TestPolicyCreateOmitsEmptyNetworkAllowlist(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("policies.manage"))
	mux.HandleFunc("POST /policies", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"p9","name":"Plain","current_version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Plain")
	form.Set("network_allowlist", "   \n , \n")
	rec := authedPostForm(t, s, "/policies", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	var got struct {
		Boundary struct {
			Rules map[string]any `json:"rules"`
		} `json:"boundary"`
	}
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode body: %v (%s)", err, gotBody)
	}
	if got.Boundary.Rules != nil {
		t.Errorf("boundary.rules = %v, want unset for an empty allowlist", got.Boundary.Rules)
	}
}

// TestPolicyDetailShowsAndEditsPermittedRoles asserts the policy detail page
// resolves permitted-role ids to names, offers the Edit affordance for a
// policies.manage caller, and notes that boundary changes go through policyctl.
func TestPolicyDetailShowsAndEditsPermittedRoles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("policies.manage", "roles.manage"))
	mux.HandleFunc("GET /policies/{policyId}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"p1","name":"Production","current_version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /policies/{policyId}/versions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	mux.HandleFunc("GET /policies/{policyId}/roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"role_ids":["r-deploy"]}`))
	})
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rolesJSON())
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/policies/p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Permitted roles", "Deployer", // permitted role id resolved to its name
		"Edit permitted roles", `data-uk-modal-open="policy-roles-p1"`, // gated edit
		"policyctl", // the boundary-changes note
	} {
		if !strings.Contains(body, want) {
			t.Errorf("policy detail missing %q", want)
		}
	}
}

// TestPolicyDetailPermittedRolesReadOnlyWithoutManage asserts a caller lacking
// policies.manage sees the permitted roles but not the Edit affordance.
func TestPolicyDetailPermittedRolesReadOnlyWithoutManage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	mux.HandleFunc("GET /policies/{policyId}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"p1","name":"Production","current_version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /policies/{policyId}/versions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	mux.HandleFunc("GET /policies/{policyId}/roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"role_ids":[]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/policies/p1")
	body := rec.Body.String()
	if !strings.Contains(body, "Permitted roles") {
		t.Errorf("read-only viewer should still see the permitted-roles section")
	}
	if strings.Contains(body, "Edit permitted roles") {
		t.Errorf("edit affordance must be hidden without policies.manage")
	}
}

// TestIdentityShowsNameEmailOrgPlainText asserts the top bar renders the signed-in
// user's name/email and org name in plain text (never a local:<uuid> subject as
// the primary label), sourced from the enriched whoami.
func TestIdentityShowsNameEmailOrgPlainText(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subject":"local:5f3a","organization_id":"org-9","name":"Ada Lovelace","email":"ada@example.com","org_name":"Acme Inc","org_slug":"acme","roles":["admin"],"permissions":[]}`))
	})
	mux.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	s := newServerCP(t, mux)

	rec := authedGet(t, s, "/policies")
	body := rec.Body.String()
	for _, want := range []string{"Ada Lovelace \u2014 ada@example.com", "Acme Inc"} {
		if !strings.Contains(body, want) {
			t.Errorf("identity display missing %q", want)
		}
	}
	// The raw local:<uuid> subject must not be the visible collapsed label.
	if strings.Contains(body, `>local:5f3a</summary>`) {
		t.Errorf("raw subject must not be the primary user-menu label")
	}
}
