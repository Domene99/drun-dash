package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// policiesHandler mocks the /policies endpoints, capturing the Authorization
// header, raw query string, and path so tests can assert the Bearer token and
// pagination / path pass-through.
func policiesHandler(t *testing.T, gotAuth, gotQuery, gotPath *string, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
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

func TestPoliciesListNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir()) // empty dir: no token

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestPoliciesGetNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "get", "pol_1"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestPoliciesVersionsNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "versions", "pol_1"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestPoliciesListSuccessWithPagination(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, policyList{
			Items: []policy{
				{ID: "pol_1", Name: "prod-guardrails", Description: "tight prod boundary", CurrentVersion: 4, CreatedAt: "2026-01-01T00:00:00Z"},
				{ID: "pol_2", Name: "sandbox", CurrentVersion: 1, CreatedAt: "2026-01-02T00:00:00Z"},
			},
			NextCursor: "cursor-XYZ",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "the-token", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "list", "--limit", "10", "--cursor", "prev-cursor"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer the-token" {
		t.Errorf("Authorization = %q, want 'Bearer the-token'", gotAuth)
	}
	if gotPath != "/policies" {
		t.Errorf("path = %q, want /policies", gotPath)
	}
	// --limit / --cursor must be passed through as query params.
	if !strings.Contains(gotQuery, "limit=10") || !strings.Contains(gotQuery, "cursor=prev-cursor") {
		t.Errorf("query = %q, want limit=10 & cursor=prev-cursor", gotQuery)
	}
	for _, want := range []string{"pol_1", "prod-guardrails", "tight prod boundary", "pol_2", "sandbox"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
	// next_cursor present => a "more available" hint including the cursor.
	if !strings.Contains(out.String(), "cursor-XYZ") || !strings.Contains(strings.ToLower(out.String()), "more available") {
		t.Errorf("stdout %q missing next-page hint", out.String())
	}
}

func TestPoliciesListNoQueryWhenFlagsUnset(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, policyList{Items: []policy{{ID: "pol_1", Name: "only", CurrentVersion: 1, CreatedAt: "2026-01-01T00:00:00Z"}}})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "list"}, &out, &errOut)
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

func TestPoliciesGetSuccess(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, policy{
			ID:             "pol_42",
			Name:           "prod-guardrails",
			Description:    "tight prod boundary",
			CurrentVersion: 7,
			CreatedAt:      "2026-02-01T00:00:00Z",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tok-get", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "get", "pol_42"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tok-get" {
		t.Errorf("Authorization = %q, want 'Bearer tok-get'", gotAuth)
	}
	if gotPath != "/policies/pol_42" {
		t.Errorf("path = %q, want /policies/pol_42", gotPath)
	}
	for _, want := range []string{"pol_42", "prod-guardrails", "tight prod boundary", "7", "2026-02-01T00:00:00Z"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
}

func TestPoliciesGetNotFound(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apiError{Code: "not_found", Message: "no such policy"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "get", "missing"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(errOut.String()), "not found") {
		t.Errorf("stderr = %q, want a not-found message", errOut.String())
	}
}

func TestPoliciesGetMissingArg(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "get"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "policyId") {
		t.Errorf("stderr = %q, want a <policyId> required message", errOut.String())
	}
}

func TestPoliciesVersionsSuccess(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, policyVersionList{
			Items: []policyVersion{
				{
					PolicyID:  "pol_42",
					Version:   1,
					Digest:    "sha256:aaa111",
					Boundary:  json.RawMessage(`{"network":"none"}`),
					Signing:   signingMetadata{Algorithm: "ed25519", KeyID: "key-1", Signature: "sig-1", SignedAt: "2026-01-01T00:00:00Z"},
					CreatedAt: "2026-01-01T00:00:00Z",
				},
				{
					PolicyID:  "pol_42",
					Version:   2,
					Digest:    "sha256:bbb222",
					Signing:   signingMetadata{Algorithm: "ed25519", KeyID: "key-2", Signature: "sig-2"},
					CreatedAt: "2026-02-01T00:00:00Z",
				},
			},
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tok-v", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "versions", "pol_42"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tok-v" {
		t.Errorf("Authorization = %q, want 'Bearer tok-v'", gotAuth)
	}
	if gotPath != "/policies/pol_42/versions" {
		t.Errorf("path = %q, want /policies/pol_42/versions", gotPath)
	}
	// Each version's digest and version number must be shown.
	for _, want := range []string{"1", "sha256:aaa111", "ed25519", "key-1", "2", "sha256:bbb222", "key-2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
}

func TestPoliciesVersionsNotFound(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, apiError{Code: "not_found", Message: "no such policy"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "versions", "missing"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(errOut.String()), "not found") {
		t.Errorf("stderr = %q, want a not-found message", errOut.String())
	}
}

func TestPoliciesVersionsMissingArg(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "versions"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "policyId") {
		t.Errorf("stderr = %q, want a <policyId> required message", errOut.String())
	}
}

// A non-2xx (non-404) response is surfaced via the flat Error's code+message.
func TestPoliciesListErrorEnvelope(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, apiError{Code: "unauthorized", Message: "token expired"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "stale", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "list"}, &out, &errOut)
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
func TestPoliciesListServerFlagOverride(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	withAuthClient(t, mockClient(policiesHandler(t, &gotAuth, &gotQuery, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, policyList{Items: []policy{{ID: "pol_1", Name: "n", CurrentVersion: 1, CreatedAt: "2026-01-01T00:00:00Z"}}})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", "http://stored.invalid")

	var out, errOut bytes.Buffer
	code := Run([]string{"policies", "list", "--server", mockBase}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer tk" {
		t.Errorf("Authorization = %q, want 'Bearer tk'", gotAuth)
	}
}

func TestPoliciesUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"policies"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"list", "get", "versions"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("policies usage %q missing %q", errOut.String(), want)
		}
	}
}
