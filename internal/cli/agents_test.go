package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// agentsHandler mocks the /agents endpoints, capturing the Authorization
// header and the raw query string so tests can assert the Bearer token and
// pagination pass-through.
func agentsHandler(t *testing.T, gotAuth, gotQuery, gotPath *string, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		*gotAuth = r.Header.Get("Authorization")
		*gotQuery = r.URL.RawQuery
		*gotPath = r.URL.Path
		respond(w, r)
	})
}

func intPtr(v int) *int { return &v }

func TestAgentsListNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir()) // empty dir: no token

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestAgentsGetNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "get", "agt_1"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestAgentsListSuccessWithPagination(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(agentsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, agentList{
			Items: []agent{
				{ID: "agt_1", Name: "planner", Description: "plans work", LatestVersion: intPtr(3), CreatedAt: "2026-01-01T00:00:00Z"},
				{ID: "agt_2", Name: "builder", CreatedAt: "2026-01-02T00:00:00Z"},
			},
			NextCursor: "cursor-XYZ",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "the-token", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "list", "--limit", "10", "--cursor", "prev-cursor"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer the-token" {
		t.Errorf("Authorization = %q, want 'Bearer the-token'", gotAuth)
	}
	if gotPath != "/agents" {
		t.Errorf("path = %q, want /agents", gotPath)
	}
	// --limit / --cursor must be passed through as query params.
	if !strings.Contains(gotQuery, "limit=10") || !strings.Contains(gotQuery, "cursor=prev-cursor") {
		t.Errorf("query = %q, want limit=10 & cursor=prev-cursor", gotQuery)
	}
	for _, want := range []string{"agt_1", "planner", "plans work", "agt_2", "builder"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
	// next_cursor present => a "more available" hint including the cursor.
	if !strings.Contains(out.String(), "cursor-XYZ") || !strings.Contains(strings.ToLower(out.String()), "more available") {
		t.Errorf("stdout %q missing next-page hint", out.String())
	}
}

func TestAgentsListNoQueryWhenFlagsUnset(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(agentsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, agentList{Items: []agent{{ID: "agt_1", Name: "only", CreatedAt: "2026-01-01T00:00:00Z"}}})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "list"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty when no pagination flags", gotQuery)
	}
	// No next_cursor => no "more available" hint.
	if strings.Contains(strings.ToLower(out.String()), "more available") {
		t.Errorf("stdout %q should not hint at more pages", out.String())
	}
}

func TestAgentsGetSuccess(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(agentsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, agent{
			ID:            "agt_42",
			Name:          "reviewer",
			Description:   "reviews diffs",
			LatestVersion: intPtr(7),
			CreatedAt:     "2026-02-01T00:00:00Z",
			UpdatedAt:     "2026-03-01T00:00:00Z",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tok-get", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "get", "agt_42"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tok-get" {
		t.Errorf("Authorization = %q, want 'Bearer tok-get'", gotAuth)
	}
	if gotPath != "/agents/agt_42" {
		t.Errorf("path = %q, want /agents/agt_42", gotPath)
	}
	for _, want := range []string{"agt_42", "reviewer", "reviews diffs", "7", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
}

func TestAgentsGetNotFound(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(agentsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apiError{Code: "not_found", Message: "no such agent"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "get", "missing"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(errOut.String()), "not found") {
		t.Errorf("stderr = %q, want a not-found message", errOut.String())
	}
}

func TestAgentsGetMissingArg(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "get"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "agentId") {
		t.Errorf("stderr = %q, want an <agentId> required message", errOut.String())
	}
}

// A non-2xx (non-404) response is surfaced via the flat Error's code+message.
func TestAgentsListErrorEnvelope(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(agentsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, apiError{Code: "unauthorized", Message: "token expired"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "stale", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"unauthorized", "token expired"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, want %q from flat Error", errOut.String(), want)
		}
	}
}

// The --server flag overrides the URL stored with the token.
func TestAgentsListServerFlagOverride(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(agentsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, agentList{Items: []agent{{ID: "agt_1", Name: "n", CreatedAt: "2026-01-01T00:00:00Z"}}})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", "http://stored.invalid")

	var out, errOut bytes.Buffer
	code := Run([]string{"agents", "list", "--server", mockBase}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tk" {
		t.Errorf("Authorization = %q, want 'Bearer tk'", gotAuth)
	}
}

func TestAgentsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"agents"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"list", "get"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("agents usage %q missing %q", errOut.String(), want)
		}
	}
}
