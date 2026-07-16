package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// These tests drive the Batch-29 model-endpoints page and the policy-create
// action through the handlers against an in-memory mock control plane (the same
// socket-free roundtripper newServerCP uses), so they run even where binding a
// port is denied (the drun sandbox). The admin UI mounts endpoints at /endpoints
// but the control-plane resource is /model-endpoints; the stored API key is
// write-only and never rendered back.

func endpointsJSON() string {
	return `{"items":[` +
		`{"id":"e1","org_id":"org-1","name":"Anthropic prod","provider":"anthropic","base_url":"https://api.anthropic.com","active":true,"secret_set":true,"created_at":"2026-07-01T00:00:00Z"},` +
		`{"id":"e2","org_id":"org-1","name":"OpenAI dev","provider":"openai","base_url":"https://api.openai.com","active":false,"secret_set":false,"created_at":"2026-07-02T00:00:00Z"}` +
		`]}`
}

func TestEndpointsPageListsAndGates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("endpoints.manage"))
	mux.HandleFunc("GET /model-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(endpointsJSON()))
	})
	s := newServerCP(t, mux)

	body := authedGet(t, s, "/endpoints").Body.String()
	for _, want := range []string{
		"Endpoints", "New endpoint",
		"Anthropic prod", "anthropic", "https://api.anthropic.com",
		"Key configured", "No key",
		`data-uk-modal-open="endpoint-create"`,
		`data-uk-modal-open="endpoint-edit-e1"`, `data-uk-modal-open="endpoint-delete-e1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoints page missing %q", want)
		}
	}
	// The sealed key is never rendered back to the browser.
	if strings.Contains(body, "api_key") && strings.Contains(body, "value=\"sk-") {
		t.Errorf("endpoints page must never render a stored API key")
	}
}

func TestEndpointsPageNoAccessWithoutPermission(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	s := newServerCP(t, mux)

	body := authedGet(t, s, "/endpoints").Body.String()
	if !strings.Contains(body, "No access") {
		t.Errorf("expected a no-access notice without endpoints.manage")
	}
	if strings.Contains(body, "New endpoint") {
		t.Errorf("management actions must be hidden without endpoints.manage")
	}
}

func TestEndpointCreatePostsAndRedirects(t *testing.T) {
	var createBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("endpoints.manage"))
	mux.HandleFunc("POST /model-endpoints", func(w http.ResponseWriter, r *http.Request) {
		createBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"e9","name":"New","provider":"anthropic","base_url":"https://x","active":true,"secret_set":true,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Anthropic prod")
	form.Set("provider", "anthropic")
	form.Set("base_url", "https://api.anthropic.com")
	form.Set("api_key", "sk-secret-123")
	rec := authedPostForm(t, s, "/endpoints", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/endpoints" {
		t.Errorf("Location = %q, want /endpoints", loc)
	}
	var got EndpointCreate
	if err := json.Unmarshal(createBody, &got); err != nil {
		t.Fatalf("decode create body: %v (%s)", err, createBody)
	}
	if got.Name != "Anthropic prod" || got.Provider != "anthropic" || got.BaseURL != "https://api.anthropic.com" || got.APIKey != "sk-secret-123" {
		t.Errorf("create body = %+v", got)
	}
}

func TestEndpointCreateRequiresNameAndBaseURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("endpoints.manage"))
	mux.HandleFunc("GET /model-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("provider", "anthropic")
	form.Set("base_url", "https://api.anthropic.com")
	rec := authedPostForm(t, s, "/endpoints", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "name is required") {
		t.Errorf("expected a name-required message:\n%s", rec.Body.String())
	}
}

func TestEndpointUpdateOmitsBlankKeyButSendsOthers(t *testing.T) {
	var updateBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("endpoints.manage"))
	mux.HandleFunc("PATCH /model-endpoints/{id}", func(w http.ResponseWriter, r *http.Request) {
		updateBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"e1","name":"Renamed","provider":"anthropic","base_url":"https://x","active":true,"secret_set":true,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	// A blank API key is omitted so the stored key is left untouched.
	form := url.Values{}
	form.Set("name", "Renamed")
	form.Set("provider", "anthropic")
	form.Set("base_url", "https://x")
	form.Set("active", "true")
	form.Set("api_key", "   ")
	rec := authedPostForm(t, s, "/endpoints/e1/update", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	var got EndpointUpdate
	if err := json.Unmarshal(updateBody, &got); err != nil {
		t.Fatalf("decode update body: %v (%s)", err, updateBody)
	}
	if got.APIKey != nil {
		t.Errorf("blank api_key must be omitted, got %q", *got.APIKey)
	}
	if got.Name == nil || *got.Name != "Renamed" || got.Active == nil || !*got.Active {
		t.Errorf("update body = %+v", got)
	}

	// A supplied key is sent so the server re-seals it.
	form.Set("api_key", "sk-new")
	_ = authedPostForm(t, s, "/endpoints/e1/update", form)
	if err := json.Unmarshal(updateBody, &got); err != nil {
		t.Fatalf("decode update body: %v", err)
	}
	if got.APIKey == nil || *got.APIKey != "sk-new" {
		t.Errorf("supplied api_key must be sent, got %v", got.APIKey)
	}
}

func TestEndpointDeleteRedirects(t *testing.T) {
	deleted := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("endpoints.manage"))
	mux.HandleFunc("DELETE /model-endpoints/{id}", func(w http.ResponseWriter, _ *http.Request) {
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	s := newServerCP(t, mux)

	rec := authedPostForm(t, s, "/endpoints/e1/delete", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if !deleted {
		t.Errorf("delete must call DELETE /model-endpoints/{id}")
	}
}

// --- Policy create ----------------------------------------------------------

func TestPoliciesPageShowsNewPolicyForManager(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("policies.manage"))
	mux.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	s := newServerCP(t, mux)

	body := authedGet(t, s, "/policies").Body.String()
	if !strings.Contains(body, "New policy") || !strings.Contains(body, `data-uk-modal-open="policy-create"`) {
		t.Errorf("policies page should offer New policy for a manager:\n%s", body)
	}

	// Without policies.manage the create action disappears.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("GET /auth/whoami", whoamiHandler("audit.view"))
	mux2.HandleFunc("GET /policies", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	viewOnly := authedGet(t, newServerCP(t, mux2), "/policies").Body.String()
	if strings.Contains(viewOnly, "New policy") {
		t.Errorf("New policy must be hidden without policies.manage")
	}
}

func TestPolicyCreatePostsBoundaryAndRedirects(t *testing.T) {
	var createBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("policies.manage"))
	mux.HandleFunc("POST /policies", func(w http.ResponseWriter, r *http.Request) {
		createBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"p9","name":"Read-only","current_version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Read-only")
	form.Set("description", "No writes")
	form.Set("allowed_tools", "search, fetch\nread")
	form.Set("network", "allowlist")
	form.Set("fan_out_ceiling", "2")
	rec := authedPostForm(t, s, "/policies", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/policies" {
		t.Errorf("Location = %q, want /policies", loc)
	}
	var got PolicyCreate
	if err := json.Unmarshal(createBody, &got); err != nil {
		t.Fatalf("decode create body: %v (%s)", err, createBody)
	}
	if got.Name != "Read-only" || got.Description != "No writes" {
		t.Errorf("policy meta = %+v", got)
	}
	b := got.Boundary
	if b.Network != "allowlist" || b.FanOutCeiling == nil || *b.FanOutCeiling != 2 {
		t.Errorf("boundary = %+v", b)
	}
	if len(b.AllowedTools) != 3 || b.AllowedTools[0] != "search" || b.AllowedTools[2] != "read" {
		t.Errorf("boundary allowed_tools = %v", b.AllowedTools)
	}
}

// --- Agent-create endpoint/policy pinning -----------------------------------

func TestAgentCreatePinsEndpointAndPolicyVersion(t *testing.T) {
	var versionBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/whoami", whoamiHandler("agents.manage"))
	mux.HandleFunc("POST /agents", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ag-9","name":"Release Bot","created_at":"2026-07-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /policies/{id}/versions", func(w http.ResponseWriter, _ *http.Request) {
		// Two versions; the latest (v2) carries the id the agent pins.
		_, _ = w.Write([]byte(`{"items":[` +
			`{"id":"pv-1","policy_id":"p1","version":1,"digest":"d1","signing":{"algorithm":"ed25519","key_id":"k","signature":"s"},"created_at":"2026-07-01T00:00:00Z"},` +
			`{"id":"pv-2","policy_id":"p1","version":2,"digest":"d2","signing":{"algorithm":"ed25519","key_id":"k","signature":"s"},"created_at":"2026-07-02T00:00:00Z"}` +
			`]}`))
	})
	mux.HandleFunc("POST /agents/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		versionBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_id":"ag-9","version":1,"created_at":"2026-07-01T00:00:00Z"}`))
	})
	s := newServerCP(t, mux)

	form := url.Values{}
	form.Set("name", "Release Bot")
	form.Set("system_prompt", "You are a release bot.")
	form.Set("fan_out_ceiling", "0")
	form.Set("default_policy_id", "p1")
	form.Set("endpoint_id", "e1")
	rec := authedPostForm(t, s, "/agents", form)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	var got AgentVersionCreate
	if err := json.Unmarshal(versionBody, &got); err != nil {
		t.Fatalf("decode version body: %v (%s)", err, versionBody)
	}
	if got.EndpointID != "e1" {
		t.Errorf("endpoint_id = %q, want e1", got.EndpointID)
	}
	if got.PolicyVersionID != "pv-2" {
		t.Errorf("policy_version_id = %q, want pv-2 (latest version's id)", got.PolicyVersionID)
	}
}
