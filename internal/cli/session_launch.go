package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// This file implements the write side of the session command tree — launching
// and terminating governed sessions — talking to the control-plane endpoints
// described in openapi/v1.yaml:
//
//   - POST /sessions/launch                  (launchSession)    -> 201 SessionLaunchResponse
//   - POST /sessions/{sessionId}/terminate   (terminateSession) -> 200 (idempotent)
//
// Both send Authorization: Bearer <token> using the token stored by `login`
// (loaded via loadToken), reuse the --server / env base-URL mechanism, and
// surface non-2xx bodies via the flat Error envelope — mirroring the read-only
// `sessions list/get` commands. They live under the same `sessions`/`session`
// verb (see sessionsCmd in sessions.go).

// sessionLaunchRequest mirrors the SessionLaunchRequest schema in
// openapi/v1.yaml. agent_id is required; the optional integers are pointers so
// "unset" is distinguishable from a zero value and omitted from the wire body.
type sessionLaunchRequest struct {
	AgentID         string            `json:"agent_id"`
	AgentVersion    *int              `json:"agent_version,omitempty"`
	RequestedFanOut *int              `json:"requested_fan_out,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
}

// sessionLaunchResponse mirrors the SessionLaunchResponse schema. policy_bundle
// is kept as json.RawMessage: it is large and only surfaced via --json, so the
// CLI need not model its shape. proxy_token is the scoped LLM-proxy credential —
// a secret shown once and never written to a log file.
type sessionLaunchResponse struct {
	SessionID     string          `json:"session_id"`
	PolicyBundle  json.RawMessage `json:"policy_bundle,omitempty"`
	ProxyBaseURL  string          `json:"proxy_base_url"`
	ProxyToken    string          `json:"proxy_token"`
	FanOutCeiling int             `json:"fan_out_ceiling"`
	ExpiresAt     string          `json:"expires_at,omitempty"`
}

// sessionLaunchCmd calls POST /sessions/launch and prints a readable summary of
// the launched session (or the full response with --json). A 403 prints a clear
// "not entitled" message and exits non-zero.
func sessionLaunchCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session launch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	agentID := fs.String("agent", "", "agent id to launch (required)")
	agentVersion := fs.Int("agent-version", 0, "pin to a specific agent version (default: latest)")
	fanOut := fs.Int("fan-out", -1, "desired concurrent sub-agent count, clamped to policy/entitlement (default: server-chosen)")
	jsonOut := fs.Bool("json", false, "dump the full SessionLaunchResponse as JSON (includes the secret proxy_token) for scripting")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash session launch --agent <id> [flags]

Launch a governed session via POST /sessions/launch, using the stored session
token. Prints a summary (session id, proxy base URL, fan-out ceiling, expiry)
plus the scoped proxy_token, which is a secret shown once — it is not written to
any file. Use --json to dump the full response (including the policy bundle and
proxy_token) for scripting.

Flags:
  --agent ID          agent id to launch (required)
  --agent-version N   pin to a specific agent version (default: latest)
  --fan-out N         desired concurrent sub-agent count, clamped to policy/entitlement
  --json              dump the full SessionLaunchResponse as JSON (includes the secret proxy_token)
  --server URL        control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *agentID == "" {
		fmt.Fprintln(stderr, "drun-dash session launch: --agent <id> is required")
		fs.Usage()
		return 2
	}

	req := sessionLaunchRequest{AgentID: *agentID}
	if *agentVersion > 0 {
		req.AgentVersion = agentVersion
	}
	if *fanOut >= 0 {
		req.RequestedFanOut = fanOut
	}

	tok, ok := loadToken("session launch", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	resp, err := launchSession(hc, base, tok.AccessToken, req)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash session launch: %v\n", err)
		return 1
	}

	if *jsonOut {
		if err := printLaunchJSON(stdout, resp); err != nil {
			fmt.Fprintf(stderr, "drun-dash session launch: %v\n", err)
			return 1
		}
		return 0
	}
	printLaunchSummary(stdout, resp)
	return 0
}

// sessionTerminateCmd calls POST /sessions/{sessionId}/terminate and reports
// success. The endpoint is idempotent; an unknown or forbidden session surfaces
// the flat Error envelope and exits non-zero.
func sessionTerminateCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session terminate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash session terminate <sessionId> [flags]

Terminate a governed session via POST /sessions/{sessionId}/terminate, using the
stored session token. Idempotent once the session has reached a terminal state.

Flags:
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "drun-dash session terminate: exactly one <sessionId> argument is required")
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)

	tok, ok := loadToken("session terminate", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	if err := terminateSession(hc, base, tok.AccessToken, id); err != nil {
		fmt.Fprintf(stderr, "drun-dash session terminate: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Session %s terminated.\n", id)
	return 0
}

// launchSession performs POST /sessions/launch with the Bearer token. A 403 is
// reported as a clear "not entitled" error; other non-201 statuses are surfaced
// via the flat Error envelope.
func launchSession(hc *http.Client, base, token string, req sessionLaunchRequest) (*sessionLaunchResponse, error) {
	body, status, err := postJSONAuth(hc, base+"/sessions/launch", token, req)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusCreated:
		var r sessionLaunchResponse
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("decode launch response: %w", err)
		}
		return &r, nil
	case http.StatusForbidden:
		if e, ok := decodeAPIError(body); ok && e.Message != "" {
			return nil, fmt.Errorf("not entitled to launch agent %q: %s", req.AgentID, e.Message)
		}
		return nil, fmt.Errorf("not entitled to launch agent %q", req.AgentID)
	default:
		return nil, apiErrorFromResponse(status, body)
	}
}

// terminateSession performs POST /sessions/{sessionId}/terminate with the Bearer
// token. A 200 (the idempotent terminal state) is success; any other status is
// surfaced via the flat Error envelope.
func terminateSession(hc *http.Client, base, token, id string) error {
	body, status, err := postJSONAuth(hc, base+"/sessions/"+url.PathEscape(id)+"/terminate", token, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	return apiErrorFromResponse(status, body)
}

// postJSONAuth issues an authenticated POST and returns the (capped) body and
// status, mirroring getJSON. reqBody is marshaled to JSON when non-nil; a nil
// body sends an empty request (used by terminate).
func postJSONAuth(hc *http.Client, u, token string, reqBody any) ([]byte, int, error) {
	var r io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, 0, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, u, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

// printLaunchSummary renders the launch result as a readable summary. The
// proxy_token is a secret shown once; it is printed to stdout for the operator
// but never written to a log file.
func printLaunchSummary(w io.Writer, r *sessionLaunchResponse) {
	fmt.Fprintln(w, "Session launched.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Session ID:       %s\n", r.SessionID)
	fmt.Fprintf(w, "Proxy base URL:   %s\n", r.ProxyBaseURL)
	fmt.Fprintf(w, "Fan-out ceiling:  %d\n", r.FanOutCeiling)
	if r.ExpiresAt != "" {
		fmt.Fprintf(w, "Expires at:       %s\n", r.ExpiresAt)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Proxy token (secret — shown once, not written to any file):")
	fmt.Fprintf(w, "  %s\n", r.ProxyToken)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Re-run with --json to dump the full response (including the policy bundle) for scripting.")
}

// printLaunchJSON dumps the full SessionLaunchResponse as indented JSON,
// preserving the policy_bundle verbatim. The dump goes to stdout only and
// includes the secret proxy_token for scripting — it is never written to a file.
func printLaunchJSON(w io.Writer, r *sessionLaunchResponse) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("encode launch response: %w", err)
	}
	return nil
}
