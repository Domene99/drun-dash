package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// spendHandler mocks the /analytics/spend endpoint, capturing the Authorization
// header, raw query string, and path so tests can assert the Bearer token and
// query pass-through.
func spendHandler(t *testing.T, gotAuth, gotQuery, gotPath *string, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
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

func int64Ptr(v int64) *int64 { return &v }

func TestSpendNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir()) // empty dir: no token

	var out, errOut bytes.Buffer
	code := Run([]string{"spend"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestSpendSuccessWithFlags(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(spendHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, spendReport{
			From:     "2026-01-01T00:00:00Z",
			To:       "2026-02-01T00:00:00Z",
			Currency: "USD",
			Total:    12.50,
			Groups: []spendGroup{
				{Key: "agt_1", Amount: 10.00, Tokens: int64Ptr(1000)},
				{Key: "agt_2", Amount: 2.50},
			},
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "the-token", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"spend", "--from", "2026-01-01T00:00:00Z", "--to", "2026-02-01T00:00:00Z", "--group-by", "agent"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer the-token" {
		t.Errorf("Authorization = %q, want 'Bearer the-token'", gotAuth)
	}
	if gotPath != "/analytics/spend" {
		t.Errorf("path = %q, want /analytics/spend", gotPath)
	}
	for _, want := range []string{"from=2026-01-01T00%3A00%3A00Z", "to=2026-02-01T00%3A00%3A00Z", "group_by=agent"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query = %q, want to contain %q", gotQuery, want)
		}
	}
	for _, want := range []string{"USD", "12.50", "agt_1", "10.00", "1000", "agt_2", "2.50"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
}

func TestSpendNoQueryWhenFlagsUnset(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(spendHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, spendReport{From: "a", To: "b", Currency: "USD", Total: 0, Groups: nil})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"spend"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty when no flags", gotQuery)
	}
	if !strings.Contains(strings.ToLower(out.String()), "no spend groups") {
		t.Errorf("stdout %q missing empty-groups notice", out.String())
	}
}

// A 400 for bad flags surfaces the server's flat Error (code + message).
func TestSpendBadRequestEnvelope(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(spendHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "group_by must be one of agent, policy, principal"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"spend", "--group-by", "bogus"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"invalid_argument", "group_by must be one of"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, want %q from flat Error", errOut.String(), want)
		}
	}
}

// The --server flag overrides the URL stored with the token.
func TestSpendServerFlagOverride(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(spendHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, spendReport{From: "a", To: "b", Currency: "USD", Total: 1, Groups: nil})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", "http://stored.invalid")

	var out, errOut bytes.Buffer
	code := Run([]string{"spend", "--server", mockBase}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tk" {
		t.Errorf("Authorization = %q, want 'Bearer tk'", gotAuth)
	}
}
