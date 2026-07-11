package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// launchHandler mocks POST /sessions/launch, capturing the Authorization header
// and decoded request body so tests can assert the Bearer token and payload.
func launchHandler(t *testing.T, gotAuth *string, gotReq *sessionLaunchRequest, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/sessions/launch" {
			t.Errorf("path = %s, want /sessions/launch", r.URL.Path)
		}
		*gotAuth = r.Header.Get("Authorization")
		if gotReq != nil {
			if err := json.NewDecoder(r.Body).Decode(gotReq); err != nil {
				t.Errorf("decode request body: %v", err)
			}
		}
		respond(w, r)
	})
}

func TestSessionLaunchNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir()) // empty dir: no token

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "launch", "--agent", "agt_1"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

// No stored token => no request is sent: the not-logged-in path exits before
// touching the HTTP client.
func TestSessionLaunchNoRequestWhenNotLoggedIn(t *testing.T) {
	var called bool
	withAuthClient(t, mockClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})))
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	if code := Run([]string{"session", "launch", "--agent", "agt_1"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if called {
		t.Errorf("HTTP request was sent despite no stored token")
	}
}

func TestSessionLaunchMissingAgent(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "launch"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--agent") {
		t.Errorf("stderr = %q, want an --agent required message", errOut.String())
	}
}

func TestSessionLaunchSuccessSummary(t *testing.T) {
	var gotAuth string
	var gotReq sessionLaunchRequest
	withAuthClient(t, mockClient(launchHandler(t, &gotAuth, &gotReq, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, sessionLaunchResponse{
			SessionID:     "ses_new",
			PolicyBundle:  json.RawMessage(`{"policy_id":"pol_1","rules":["a","b"]}`),
			ProxyBaseURL:  "https://proxy.test",
			ProxyToken:    "secret-proxy-token",
			FanOutCeiling: 4,
			ExpiresAt:     "2026-07-11T12:00:00Z",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "launch-tok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "launch", "--agent", "agt_9", "--agent-version", "3", "--fan-out", "2"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer launch-tok" {
		t.Errorf("Authorization = %q, want 'Bearer launch-tok'", gotAuth)
	}
	if gotReq.AgentID != "agt_9" {
		t.Errorf("agent_id = %q, want agt_9", gotReq.AgentID)
	}
	if gotReq.AgentVersion == nil || *gotReq.AgentVersion != 3 {
		t.Errorf("agent_version = %v, want 3", gotReq.AgentVersion)
	}
	if gotReq.RequestedFanOut == nil || *gotReq.RequestedFanOut != 2 {
		t.Errorf("requested_fan_out = %v, want 2", gotReq.RequestedFanOut)
	}
	// Summary surfaces the key fields and the secret proxy token.
	for _, want := range []string{"ses_new", "https://proxy.test", "4", "2026-07-11T12:00:00Z", "secret-proxy-token"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
	// The default summary must NOT dump the large policy bundle.
	if strings.Contains(out.String(), "rules") {
		t.Errorf("summary should not dump the policy bundle, got %q", out.String())
	}
}

// Optional flags omitted => the request carries no agent_version / requested_fan_out.
func TestSessionLaunchOmitsUnsetOptionals(t *testing.T) {
	var gotAuth string
	var gotReq sessionLaunchRequest
	withAuthClient(t, mockClient(launchHandler(t, &gotAuth, &gotReq, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, sessionLaunchResponse{SessionID: "ses_x", ProxyBaseURL: "https://p", ProxyToken: "tk", FanOutCeiling: 1})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	if code := Run([]string{"session", "launch", "--agent", "agt_1"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotReq.AgentVersion != nil {
		t.Errorf("agent_version = %v, want nil when --agent-version unset", *gotReq.AgentVersion)
	}
	if gotReq.RequestedFanOut != nil {
		t.Errorf("requested_fan_out = %v, want nil when --fan-out unset", *gotReq.RequestedFanOut)
	}
}

func TestSessionLaunchJSONDumpsFullResponse(t *testing.T) {
	var gotAuth string
	withAuthClient(t, mockClient(launchHandler(t, &gotAuth, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, sessionLaunchResponse{
			SessionID:     "ses_json",
			PolicyBundle:  json.RawMessage(`{"policy_id":"pol_1","rules":["only-anthropic"]}`),
			ProxyBaseURL:  "https://proxy.test",
			ProxyToken:    "secret-proxy-token",
			FanOutCeiling: 7,
			ExpiresAt:     "2026-07-11T12:00:00Z",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "launch", "--agent", "agt_1", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	// --json must be valid JSON carrying the full response, including the
	// policy bundle and the proxy token.
	var got sessionLaunchResponse
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out.String())
	}
	if got.SessionID != "ses_json" || got.ProxyToken != "secret-proxy-token" || got.FanOutCeiling != 7 {
		t.Errorf("decoded response = %+v, missing fields", got)
	}
	if !strings.Contains(out.String(), "only-anthropic") {
		t.Errorf("--json output %q missing policy bundle", out.String())
	}
}

// A 403 prints a clear "not entitled" message and exits non-zero.
func TestSessionLaunchForbidden(t *testing.T) {
	var gotAuth string
	withAuthClient(t, mockClient(launchHandler(t, &gotAuth, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, apiError{Code: "forbidden", Message: "no entitlement for this agent"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "launch", "--agent", "agt_forbidden"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(errOut.String()), "not entitled") {
		t.Errorf("stderr = %q, want a 'not entitled' message", errOut.String())
	}
	if !strings.Contains(errOut.String(), "no entitlement for this agent") {
		t.Errorf("stderr = %q, want the server's message", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failure", out.String())
	}
}

// terminateHandler mocks POST /sessions/{id}/terminate.
func terminateHandler(t *testing.T, gotAuth, gotPath *string, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		*gotAuth = r.Header.Get("Authorization")
		*gotPath = r.URL.Path
		respond(w, r)
	})
}

func TestSessionTerminateSuccess(t *testing.T) {
	var gotAuth, gotPath string
	withAuthClient(t, mockClient(terminateHandler(t, &gotAuth, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, session{SessionID: "ses_kill", AgentID: "agt_1", Status: "terminated", CreatedAt: "2026-01-01T00:00:00Z"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "kill-tok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "terminate", "ses_kill"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer kill-tok" {
		t.Errorf("Authorization = %q, want 'Bearer kill-tok'", gotAuth)
	}
	if gotPath != "/sessions/ses_kill/terminate" {
		t.Errorf("path = %q, want /sessions/ses_kill/terminate", gotPath)
	}
	if !strings.Contains(out.String(), "ses_kill") || !strings.Contains(strings.ToLower(out.String()), "terminated") {
		t.Errorf("stdout = %q, want a success message naming ses_kill", out.String())
	}
}

func TestSessionTerminateNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "terminate", "ses_1"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

func TestSessionTerminateMissingArg(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "terminate"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "sessionId") {
		t.Errorf("stderr = %q, want a <sessionId> required message", errOut.String())
	}
}

// An unknown/forbidden session surfaces the flat Error envelope.
func TestSessionTerminateErrorEnvelope(t *testing.T) {
	var gotAuth, gotPath string
	withAuthClient(t, mockClient(terminateHandler(t, &gotAuth, &gotPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, apiError{Code: "forbidden", Message: "not your session"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"session", "terminate", "ses_x"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"forbidden", "not your session"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, want %q from flat Error", errOut.String(), want)
		}
	}
}

// The singular `session` and plural `sessions` verbs both reach launch/terminate.
func TestSessionsPluralVerbReachesLaunch(t *testing.T) {
	var gotAuth string
	withAuthClient(t, mockClient(launchHandler(t, &gotAuth, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, sessionLaunchResponse{SessionID: "ses_1", ProxyBaseURL: "https://p", ProxyToken: "tk", FanOutCeiling: 1})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	if code := Run([]string{"sessions", "launch", "--agent", "agt_1"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "ses_1") {
		t.Errorf("stdout = %q, want ses_1", out.String())
	}
}

// Existing read-only sessions still work via the singular alias and the usage
// now lists launch/terminate.
func TestSessionsUsageListsLaunchTerminate(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"session"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"list", "get", "launch", "terminate"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("session usage %q missing %q", errOut.String(), want)
		}
	}
}
