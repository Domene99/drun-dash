package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"text/tabwriter"
)

// This file implements the read-only `sessions` command tree, talking to the
// control-plane session endpoints described in openapi/v1.yaml:
//
//   - GET /sessions              (listSessions) -> SessionList{ items, next_cursor? }
//   - GET /sessions/{sessionId}  (getSession)   -> Session
//
// It mirrors the `agents` command exactly: the same Bearer-token loading (via
// internal/clitoken), the same --server / env base-URL mechanism, the same flat
// Error decoding for non-2xx, and the same not-logged-in behavior. The write
// side (launch/terminate) lives in session_launch.go under the same verb.

// session mirrors the Session schema in openapi/v1.yaml. Optional integers are
// pointers so "unset" is distinguishable from a zero value.
type session struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	AgentVersion  *int   `json:"agent_version,omitempty"`
	PolicyID      string `json:"policy_id,omitempty"`
	PolicyVersion *int   `json:"policy_version,omitempty"`
	Status        string `json:"status"`
	FanOutCeiling int    `json:"fan_out_ceiling"`
	CreatedAt     string `json:"created_at"`
	TerminatedAt  string `json:"terminated_at,omitempty"`
}

// sessionList mirrors the SessionList schema: a page of sessions plus the Page
// envelope's optional next_cursor.
type sessionList struct {
	Items      []session `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// sessionsCmd dispatches the `sessions`/`session` subcommands. `list` and `get`
// are read-only (this file); `launch` and `terminate` drive session lifecycle
// (session_launch.go). Both the plural and singular verbs reach this dispatcher.
func sessionsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		sessionsUsage(stderr)
		return 2
	}
	switch sub := args[0]; sub {
	case "list":
		return sessionsListCmd(args[1:], stdout, stderr)
	case "get":
		return sessionsGetCmd(args[1:], stdout, stderr)
	case "launch":
		return sessionLaunchCmd(args[1:], stdout, stderr)
	case "terminate":
		return sessionTerminateCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		sessionsUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "drun-dash sessions: unknown subcommand %q\n\n", sub)
		sessionsUsage(stderr)
		return 2
	}
}

// sessionsUsage writes help for the `sessions`/`session` command tree.
func sessionsUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: drun-dash sessions <subcommand> [flags]

Manage governed agent sessions. (The singular "session" is an alias.)

Subcommands:
  list                      List sessions (GET /sessions)
  get <sessionId>           Show a single session's detail (GET /sessions/{sessionId})
  launch --agent <id>       Launch a governed session (POST /sessions/launch)
  terminate <sessionId>     Terminate a session (POST /sessions/{sessionId}/terminate)

Run "drun-dash sessions <subcommand> -h" for subcommand-specific flags.
`)
}

// sessionsListCmd calls GET /sessions and prints a readable table, passing
// --limit / --cursor through as query params and hinting when more pages exist.
func sessionsListCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sessions list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	limit := fs.Int("limit", 0, "maximum number of sessions to return (server default when unset)")
	cursor := fs.String("cursor", "", "pagination cursor from a previous page's next_cursor")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash sessions list [flags]

List governed sessions via GET /sessions, using the stored session token.

Flags:
  --limit N      maximum number of sessions to return (server default when unset)
  --cursor C     pagination cursor from a previous page's next_cursor
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tok, ok := loadToken("sessions list", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	list, err := fetchSessionList(hc, base, tok.AccessToken, *limit, *cursor)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash sessions list: %v\n", err)
		return 1
	}

	printSessionList(stdout, list)
	return 0
}

// sessionsGetCmd calls GET /sessions/{sessionId} and prints the session detail.
// A 404 prints a clear not-found message and exits non-zero.
func sessionsGetCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sessions get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash sessions get <sessionId> [flags]

Fetch and print a single governed session via GET /sessions/{sessionId}, using
the stored session token.

Flags:
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "drun-dash sessions get: exactly one <sessionId> argument is required")
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)

	tok, ok := loadToken("sessions get", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	s, err := fetchSession(hc, base, tok.AccessToken, id)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash sessions get: %v\n", err)
		return 1
	}

	printSessionDetail(stdout, s)
	return 0
}

// fetchSessionList performs GET /sessions with the Bearer token, passing limit
// and cursor as query params when set.
func fetchSessionList(hc *http.Client, base, token string, limit int, cursor string) (*sessionList, error) {
	u := base + "/sessions"
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	body, status, err := getJSON(hc, u, token)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiErrorFromResponse(status, body)
	}
	var list sessionList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode session list: %w", err)
	}
	return &list, nil
}

// fetchSession performs GET /sessions/{sessionId} with the Bearer token. A 404
// is reported as a clear not-found error.
func fetchSession(hc *http.Client, base, token, id string) (*session, error) {
	body, status, err := getJSON(hc, base+"/sessions/"+url.PathEscape(id), token)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var s session
		if err := json.Unmarshal(body, &s); err != nil {
			return nil, fmt.Errorf("decode session: %w", err)
		}
		return &s, nil
	case http.StatusNotFound:
		if e, ok := decodeAPIError(body); ok && e.Message != "" {
			return nil, fmt.Errorf("session %q not found: %s", id, e.Message)
		}
		return nil, fmt.Errorf("session %q not found", id)
	default:
		return nil, apiErrorFromResponse(status, body)
	}
}

// printSessionList renders the sessions as an aligned table and hints when
// another page is available.
func printSessionList(w io.Writer, list *sessionList) {
	if len(list.Items) == 0 {
		fmt.Fprintln(w, "No sessions found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION ID\tAGENT ID\tSTATUS\tFAN-OUT\tCREATED")
	for _, s := range list.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", s.SessionID, s.AgentID, s.Status, s.FanOutCeiling, s.CreatedAt)
	}
	tw.Flush()
	if list.NextCursor != "" {
		fmt.Fprintf(w, "\nMore available — re-run with --cursor %s\n", list.NextCursor)
	}
}

// printSessionDetail renders a single session's fields, omitting empty optionals.
func printSessionDetail(w io.Writer, s *session) {
	fmt.Fprintf(w, "Session ID:    %s\n", s.SessionID)
	fmt.Fprintf(w, "Agent ID:      %s\n", s.AgentID)
	if s.AgentVersion != nil {
		fmt.Fprintf(w, "Agent ver:     %d\n", *s.AgentVersion)
	}
	if s.PolicyID != "" {
		fmt.Fprintf(w, "Policy ID:     %s\n", s.PolicyID)
	}
	if s.PolicyVersion != nil {
		fmt.Fprintf(w, "Policy ver:    %d\n", *s.PolicyVersion)
	}
	fmt.Fprintf(w, "Status:        %s\n", s.Status)
	fmt.Fprintf(w, "Fan-out:       %d\n", s.FanOutCeiling)
	fmt.Fprintf(w, "Created:       %s\n", s.CreatedAt)
	if s.TerminatedAt != "" {
		fmt.Fprintf(w, "Terminated:    %s\n", s.TerminatedAt)
	}
}
