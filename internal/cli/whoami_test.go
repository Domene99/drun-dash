package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// whoamiHandler mocks GET /auth/whoami, capturing the Authorization header so
// tests can assert the Bearer token was sent.
func whoamiHandler(t *testing.T, status int, id identity, gotAuth *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/whoami" {
			t.Errorf("path = %q, want /auth/whoami", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		*gotAuth = r.Header.Get("Authorization")
		writeJSON(w, status, id)
	})
}

func TestWhoamiNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir()) // empty dir: no token

	var out, errOut bytes.Buffer
	code := Run([]string{"whoami"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestWhoamiWithStoredToken(t *testing.T) {
	var gotAuth string
	withAuthClient(t, mockClient(whoamiHandler(t, http.StatusOK, identity{
		Subject:        "idp-subject-1",
		OrganizationID: "org_123",
		Email:          "dev@example.com",
		Roles:          []string{"admin", "member"},
	}, &gotAuth)))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "the-bearer-token", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"whoami"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer the-bearer-token" {
		t.Errorf("Authorization header = %q, want 'Bearer the-bearer-token'", gotAuth)
	}
	for _, want := range []string{"idp-subject-1", "org_123", "dev@example.com", "admin", "member"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
}

// A --server flag overrides the URL stored with the token; the transport
// ignores host anyway, but the command must not fail resolving it.
func TestWhoamiServerFlagOverride(t *testing.T) {
	var gotAuth string
	withAuthClient(t, mockClient(whoamiHandler(t, http.StatusOK, identity{
		Subject:        "s",
		OrganizationID: "org_x",
		Roles:          []string{"member"},
	}, &gotAuth)))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", "http://stored.invalid")

	var out, errOut bytes.Buffer
	code := Run([]string{"whoami", "--server", mockBase}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tk" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
}

func TestWhoamiUnauthorized(t *testing.T) {
	var gotAuth string
	withAuthClient(t, mockClient(whoamiHandler(t, http.StatusUnauthorized, identity{}, &gotAuth)))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "stale", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"whoami"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "401") {
		t.Errorf("stderr = %q, want a 401 hint", errOut.String())
	}
}

func seedToken(t *testing.T, dir, accessToken, serverURL string) {
	t.Helper()
	if err := clitoken.New(dir).Save(clitoken.Token{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(time.Hour),
		ServerURL:   serverURL,
	}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
}
