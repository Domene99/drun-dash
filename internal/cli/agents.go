package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// This file implements the read-only `agents` command tree, talking to the
// control-plane catalog endpoints described in openapi/v1.yaml:
//
//   - GET /agents            (listAgents) -> AgentList{ items, next_cursor? }
//   - GET /agents/{agentId}  (getAgent)   -> Agent
//
// Both send Authorization: Bearer <token> using the token stored by `login`
// (loaded via internal/clitoken). Non-2xx responses carry the flat Error
// envelope (code + message), which is surfaced to the operator.

// agent mirrors the Agent schema in openapi/v1.yaml. latest_version is a
// pointer so "no published version" is distinguishable from version 0.
type agent struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	LatestVersion *int   `json:"latest_version,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// agentList mirrors the AgentList schema: a page of agents plus the Page
// envelope's optional next_cursor.
type agentList struct {
	Items      []agent `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// apiError mirrors the flat Error schema (code + message) returned on non-2xx.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// agentsCmd dispatches the `agents` subcommands (list, get).
func agentsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		agentsUsage(stderr)
		return 2
	}
	switch sub := args[0]; sub {
	case "list":
		return agentsListCmd(args[1:], stdout, stderr)
	case "get":
		return agentsGetCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		agentsUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "drun-dash agents: unknown subcommand %q\n\n", sub)
		agentsUsage(stderr)
		return 2
	}
}

// agentsUsage writes help for the `agents` command tree.
func agentsUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: drun-dash agents <subcommand> [flags]

Read-only access to the organization's agent catalog.

Subcommands:
  list             List catalog agents (GET /agents)
  get <agentId>    Show a single agent's detail (GET /agents/{agentId})

Run "drun-dash agents <subcommand> -h" for subcommand-specific flags.
`)
}

// agentsListCmd calls GET /agents and prints a readable table, passing
// --limit / --cursor through as query params and hinting when more pages exist.
func agentsListCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	limit := fs.Int("limit", 0, "maximum number of agents to return (server default when unset)")
	cursor := fs.String("cursor", "", "pagination cursor from a previous page's next_cursor")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash agents list [flags]

List agents in the organization's catalog via GET /agents, using the stored
session token.

Flags:
  --limit N      maximum number of agents to return (server default when unset)
  --cursor C     pagination cursor from a previous page's next_cursor
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tok, ok := loadToken("agents list", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	list, err := fetchAgentList(hc, base, tok.AccessToken, *limit, *cursor)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash agents list: %v\n", err)
		return 1
	}

	printAgentList(stdout, list)
	return 0
}

// agentsGetCmd calls GET /agents/{agentId} and prints the agent detail. A 404
// prints a clear not-found message and exits non-zero.
func agentsGetCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash agents get <agentId> [flags]

Fetch and print a single catalog agent via GET /agents/{agentId}, using the
stored session token.

Flags:
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "drun-dash agents get: exactly one <agentId> argument is required")
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)

	tok, ok := loadToken("agents get", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	a, err := fetchAgent(hc, base, tok.AccessToken, id)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash agents get: %v\n", err)
		return 1
	}

	printAgentDetail(stdout, a)
	return 0
}

// loadToken loads the stored CLI token. When no token exists it prints a clear
// "not logged in" message to stderr and returns ok=false so callers can exit
// non-zero without sending a request.
func loadToken(cmd string, stderr io.Writer) (clitoken.Token, bool) {
	store, err := clitoken.Default()
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash %s: %v\n", cmd, err)
		return clitoken.Token{}, false
	}
	tok, err := store.Load()
	if err != nil {
		if errors.Is(err, clitoken.ErrNotFound) {
			fmt.Fprintln(stderr, "Not logged in. Run `drun-dash login` first.")
			return clitoken.Token{}, false
		}
		fmt.Fprintf(stderr, "drun-dash %s: %v\n", cmd, err)
		return clitoken.Token{}, false
	}
	return tok, true
}

// fetchAgentList performs GET /agents with the Bearer token, passing limit and
// cursor as query params when set.
func fetchAgentList(hc *http.Client, base, token string, limit int, cursor string) (*agentList, error) {
	u := base + "/agents"
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
	var list agentList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode agent list: %w", err)
	}
	return &list, nil
}

// fetchAgent performs GET /agents/{agentId} with the Bearer token. A 404 is
// reported as a clear not-found error.
func fetchAgent(hc *http.Client, base, token, id string) (*agent, error) {
	body, status, err := getJSON(hc, base+"/agents/"+url.PathEscape(id), token)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var a agent
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, fmt.Errorf("decode agent: %w", err)
		}
		return &a, nil
	case http.StatusNotFound:
		if e, ok := decodeAPIError(body); ok && e.Message != "" {
			return nil, fmt.Errorf("agent %q not found: %s", id, e.Message)
		}
		return nil, fmt.Errorf("agent %q not found", id)
	default:
		return nil, apiErrorFromResponse(status, body)
	}
}

// getJSON issues an authenticated GET and returns the (capped) body and status.
func getJSON(hc *http.Client, u, token string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

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

// decodeAPIError parses the flat Error envelope, reporting ok=false when the
// body is not a well-formed Error with a code.
func decodeAPIError(body []byte) (apiError, bool) {
	var e apiError
	if err := json.Unmarshal(body, &e); err != nil || e.Code == "" {
		return apiError{}, false
	}
	return e, true
}

// apiErrorFromResponse turns a non-2xx body into an error carrying the flat
// Error's code and message, falling back to the raw status when the body is
// not the expected shape.
func apiErrorFromResponse(status int, body []byte) error {
	if e, ok := decodeAPIError(body); ok {
		if e.Message != "" {
			return fmt.Errorf("%s: %s", e.Code, e.Message)
		}
		return fmt.Errorf("%s", e.Code)
	}
	return fmt.Errorf("unexpected status %d: %s", status, strings.TrimSpace(string(body)))
}

// printAgentList renders the agents as an aligned table and hints when another
// page is available.
func printAgentList(w io.Writer, list *agentList) {
	if len(list.Items) == 0 {
		fmt.Fprintln(w, "No agents found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tLATEST\tDESCRIPTION")
	for _, a := range list.Items {
		ver := "-"
		if a.LatestVersion != nil {
			ver = strconv.Itoa(*a.LatestVersion)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.ID, a.Name, ver, a.Description)
	}
	tw.Flush()
	if list.NextCursor != "" {
		fmt.Fprintf(w, "\nMore available — re-run with --cursor %s\n", list.NextCursor)
	}
}

// printAgentDetail renders a single agent's fields, omitting empty optionals.
func printAgentDetail(w io.Writer, a *agent) {
	fmt.Fprintf(w, "ID:           %s\n", a.ID)
	fmt.Fprintf(w, "Name:         %s\n", a.Name)
	if a.Description != "" {
		fmt.Fprintf(w, "Description:  %s\n", a.Description)
	}
	if a.LatestVersion != nil {
		fmt.Fprintf(w, "Latest ver:   %d\n", *a.LatestVersion)
	}
	fmt.Fprintf(w, "Created:      %s\n", a.CreatedAt)
	if a.UpdatedAt != "" {
		fmt.Fprintf(w, "Updated:      %s\n", a.UpdatedAt)
	}
}
