package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// sessionsHandler mocks the /sessions endpoints, capturing the Authorization
// header, raw query string, and path so tests can assert the Bearer token and
// pagination pass-through.
func sessionsHandler(t *testing.T, gotAuth, gotQuery, gotPath *string, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
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

func TestSessionsListNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir()) // empty dir: no token

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestSessionsGetNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "get", "ses_1"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestSessionsListSuccessWithPagination(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(sessionsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sessionList{
			Items: []session{
				{SessionID: "ses_1", AgentID: "agt_1", AgentVersion: intPtr(3), Status: "active", FanOutCeiling: 5, CreatedAt: "2026-01-01T00:00:00Z"},
				{SessionID: "ses_2", AgentID: "agt_2", Status: "terminated", FanOutCeiling: 0, CreatedAt: "2026-01-02T00:00:00Z", TerminatedAt: "2026-01-03T00:00:00Z"},
			},
			NextCursor: "cursor-XYZ",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "the-token", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "list", "--limit", "10", "--cursor", "prev-cursor"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer the-token" {
		t.Errorf("Authorization = %q, want 'Bearer the-token'", gotAuth)
	}
	if gotPath != "/sessions" {
		t.Errorf("path = %q, want /sessions", gotPath)
	}
	if !strings.Contains(gotQuery, "limit=10") || !strings.Contains(gotQuery, "cursor=prev-cursor") {
		t.Errorf("query = %q, want limit=10 & cursor=prev-cursor", gotQuery)
	}
	for _, want := range []string{"ses_1", "agt_1", "active", "ses_2", "agt_2", "terminated"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
	if !strings.Contains(out.String(), "cursor-XYZ") || !strings.Contains(strings.ToLower(out.String()), "more available") {
		t.Errorf("stdout %q missing next-page hint", out.String())
	}
}

func TestSessionsListNoQueryWhenFlagsUnset(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(sessionsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sessionList{Items: []session{{SessionID: "ses_1", AgentID: "agt_1", Status: "pending", CreatedAt: "2026-01-01T00:00:00Z"}}})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "list"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty when no pagination flags", gotQuery)
	}
	if strings.Contains(strings.ToLower(out.String()), "more available") {
		t.Errorf("stdout %q should not hint at more pages", out.String())
	}
}

func TestSessionsGetSuccess(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(sessionsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, session{
			SessionID:     "ses_42",
			AgentID:       "agt_7",
			AgentVersion:  intPtr(2),
			Status:        "active",
			FanOutCeiling: 8,
			CreatedAt:     "2026-02-01T00:00:00Z",
			TerminatedAt:  "2026-03-01T00:00:00Z",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tok-get", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "get", "ses_42"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tok-get" {
		t.Errorf("Authorization = %q, want 'Bearer tok-get'", gotAuth)
	}
	if gotPath != "/sessions/ses_42" {
		t.Errorf("path = %q, want /sessions/ses_42", gotPath)
	}
	for _, want := range []string{"ses_42", "agt_7", "active", "8", "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
}

func TestSessionsGetNotFound(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(sessionsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apiError{Code: "not_found", Message: "no such session"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "get", "missing"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(errOut.String()), "not found") {
		t.Errorf("stderr = %q, want a not-found message", errOut.String())
	}
}

func TestSessionsGetMissingArg(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "get"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "sessionId") {
		t.Errorf("stderr = %q, want a <sessionId> required message", errOut.String())
	}
}

// A non-2xx (non-404) response is surfaced via the flat Error's code+message.
func TestSessionsListErrorEnvelope(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(sessionsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, apiError{Code: "unauthorized", Message: "token expired"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "stale", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "list"}, &out, &errOut)
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
func TestSessionsListServerFlagOverride(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(sessionsHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sessionList{Items: []session{{SessionID: "ses_1", AgentID: "agt_1", Status: "active", CreatedAt: "2026-01-01T00:00:00Z"}}})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", "http://stored.invalid")

	var out, errOut bytes.Buffer
	code := Run([]string{"sessions", "list", "--server", mockBase}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tk" {
		t.Errorf("Authorization = %q, want 'Bearer tk'", gotAuth)
	}
}

func TestSessionsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"sessions"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"list", "get"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("sessions usage %q missing %q", errOut.String(), want)
		}
	}
}
