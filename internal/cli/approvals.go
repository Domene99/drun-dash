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

// This file implements the `approvals` command tree — the human reviewer's view
// of the escalation queue — talking to the control-plane approval endpoints
// described in openapi/v1.yaml:
//
//   - GET  /approvals                        (listApprovals) -> ApprovalList{ items, next_cursor? }
//   - POST /approvals/{approvalId}/decision  (decideApproval) -> Approval (409 if already decided)
//
// It mirrors the `sessions`/`agents` commands exactly: the same Bearer-token
// loading (via internal/clitoken, through loadToken), the same --server / env
// base-URL mechanism, the same flat Error decoding for non-2xx, and the same
// not-logged-in behavior. Creating approvals is a server/agent-side action; the
// CLI focuses on the reviewer's list + decide.

// approval mirrors the Approval schema in openapi/v1.yaml. The optional fields
// (reason, decided_by, decided_at) are omitted from output when empty.
type approval struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Status    string `json:"status"`
	DecidedBy string `json:"decided_by,omitempty"`
	DecidedAt string `json:"decided_at,omitempty"`
	CreatedAt string `json:"created_at"`
}

// approvalList mirrors the ApprovalList schema: a page of approvals plus the
// Page envelope's optional next_cursor.
type approvalList struct {
	Items      []approval `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// approvalDecision mirrors the ApprovalDecision request body. decision is one of
// {approve, deny}; reason is an optional rationale for the audit trail.
type approvalDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// approvalsCmd dispatches the `approvals` subcommands (list, decide).
func approvalsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		approvalsUsage(stderr)
		return 2
	}
	switch sub := args[0]; sub {
	case "list":
		return approvalsListCmd(args[1:], stdout, stderr)
	case "decide":
		return approvalsDecideCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		approvalsUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "drun-dash approvals: unknown subcommand %q\n\n", sub)
		approvalsUsage(stderr)
		return 2
	}
}

// approvalsUsage writes help for the `approvals` command tree.
func approvalsUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: drun-dash approvals <subcommand> [flags]

Review the escalation queue: list queued approval requests and decide them.

Subcommands:
  list                                        List approval requests (GET /approvals)
  decide <approvalId> --decision approve|deny Approve or deny a request (POST /approvals/{approvalId}/decision)

Run "drun-dash approvals <subcommand> -h" for subcommand-specific flags.
`)
}

// approvalsListCmd calls GET /approvals and prints a readable table, passing
// --limit / --cursor through as query params and hinting when more pages exist.
// Pending requests (the ones awaiting a decision) are highlighted.
func approvalsListCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("approvals list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	limit := fs.Int("limit", 0, "maximum number of approvals to return (server default when unset)")
	cursor := fs.String("cursor", "", "pagination cursor from a previous page's next_cursor")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash approvals list [flags]

List approval requests in the escalation queue via GET /approvals, using the
stored session token. Pending requests (awaiting a decision) are marked.

Flags:
  --limit N      maximum number of approvals to return (server default when unset)
  --cursor C     pagination cursor from a previous page's next_cursor
  --server URL   control-plane base URL override (default: the server used at login)
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	tok, ok := loadToken("approvals list", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	list, err := fetchApprovalList(hc, base, tok.AccessToken, *limit, *cursor)
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash approvals list: %v\n", err)
		return 1
	}

	printApprovalList(stdout, list)
	return 0
}

// approvalsDecideCmd calls POST /approvals/{approvalId}/decision with the
// validated decision and optional reason. --decision must be approve|deny; an
// invalid value is a usage error. A 409 (already decided) prints a clear message
// and exits non-zero.
func approvalsDecideCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("approvals decide", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "control-plane base URL override (default: the server login used)")
	decision := fs.String("decision", "", "approve or deny (required)")
	reason := fs.String("reason", "", "optional rationale recorded in the audit trail")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash approvals decide <approvalId> --decision approve|deny [flags]

Decide a queued approval request via POST /approvals/{approvalId}/decision,
using the stored session token. A request that has already been decided returns
409 and prints a clear message.

Flags:
  --decision approve|deny   the decision to record (required)
  --reason TEXT             optional rationale recorded in the audit trail
  --server URL              control-plane base URL override (default: the server used at login)
`)
	}
	// stdlib flag.Parse stops at the first positional argument, so parse in a
	// loop to accept the <approvalId> and the flags in any order — the
	// documented form puts the id first (`decide <id> --decision approve`),
	// which a single Parse would leave the flags unread.
	id, ok := parseOnePositional(fs, args)
	if !ok {
		return 2
	}
	if id == "" {
		fmt.Fprintln(stderr, "drun-dash approvals decide: exactly one <approvalId> argument is required")
		fs.Usage()
		return 2
	}

	// Validate the decision before touching the network.
	if *decision != "approve" && *decision != "deny" {
		fmt.Fprintf(stderr, "drun-dash approvals decide: --decision must be \"approve\" or \"deny\", got %q\n", *decision)
		fs.Usage()
		return 2
	}

	tok, ok := loadToken("approvals decide", stderr)
	if !ok {
		return 1
	}
	base := resolveServer(firstNonEmpty(*server, tok.ServerURL))

	hc := newAuthHTTPClient()
	a, err := decideApproval(hc, base, tok.AccessToken, id, approvalDecision{Decision: *decision, Reason: *reason})
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash approvals decide: %v\n", err)
		return 1
	}

	printApprovalDetail(stdout, a)
	return 0
}

// parseOnePositional parses fs allowing exactly one positional argument to
// appear before, after, or between flags. It returns the positional (empty when
// none is given) and ok=false when parsing fails or more than one positional is
// supplied. The loop re-parses the tail after each positional because stdlib
// flag stops at the first non-flag token.
func parseOnePositional(fs *flag.FlagSet, args []string) (string, bool) {
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return "", false
		}
		if fs.NArg() == 0 {
			break
		}
		positionals = append(positionals, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positionals) > 1 {
		fmt.Fprintf(fs.Output(), "drun-dash approvals decide: unexpected extra arguments: %v\n", positionals[1:])
		fs.Usage()
		return "", false
	}
	if len(positionals) == 0 {
		return "", true
	}
	return positionals[0], true
}

// fetchApprovalList performs GET /approvals with the Bearer token, passing limit
// and cursor as query params when set.
func fetchApprovalList(hc *http.Client, base, token string, limit int, cursor string) (*approvalList, error) {
	u := base + "/approvals"
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
	var list approvalList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode approval list: %w", err)
	}
	return &list, nil
}

// decideApproval performs POST /approvals/{approvalId}/decision with the Bearer
// token. A 409 (the approval was already decided) is reported as a clear error;
// other non-200 statuses are surfaced via the flat Error envelope.
func decideApproval(hc *http.Client, base, token, id string, dec approvalDecision) (*approval, error) {
	body, status, err := postJSONAuth(hc, base+"/approvals/"+url.PathEscape(id)+"/decision", token, dec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var a approval
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, fmt.Errorf("decode approval: %w", err)
		}
		return &a, nil
	case http.StatusConflict:
		if e, ok := decodeAPIError(body); ok && e.Message != "" {
			return nil, fmt.Errorf("approval %q has already been decided: %s", id, e.Message)
		}
		return nil, fmt.Errorf("approval %q has already been decided", id)
	default:
		return nil, apiErrorFromResponse(status, body)
	}
}

// printApprovalList renders the approvals as an aligned table, marking pending
// requests, and hints when another page is available.
func printApprovalList(w io.Writer, list *approvalList) {
	if len(list.Items) == 0 {
		fmt.Fprintln(w, "No approvals found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "\tID\tSESSION ID\tACTION\tSTATUS\tCREATED")
	for _, a := range list.Items {
		// A caret marks the pending requests — the ones a reviewer still
		// needs to act on.
		marker := " "
		if a.Status == "pending" {
			marker = ">"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", marker, a.ID, a.SessionID, a.Action, a.Status, a.CreatedAt)
	}
	tw.Flush()
	fmt.Fprintln(w, "\n(> marks pending requests awaiting a decision)")
	if list.NextCursor != "" {
		fmt.Fprintf(w, "More available — re-run with --cursor %s\n", list.NextCursor)
	}
}

// printApprovalDetail renders a single approval's fields, omitting empty
// optionals. Used to confirm the recorded decision.
func printApprovalDetail(w io.Writer, a *approval) {
	fmt.Fprintf(w, "ID:          %s\n", a.ID)
	fmt.Fprintf(w, "Session ID:  %s\n", a.SessionID)
	fmt.Fprintf(w, "Action:      %s\n", a.Action)
	if a.Reason != "" {
		fmt.Fprintf(w, "Reason:      %s\n", a.Reason)
	}
	fmt.Fprintf(w, "Status:      %s\n", a.Status)
	if a.DecidedBy != "" {
		fmt.Fprintf(w, "Decided by:  %s\n", a.DecidedBy)
	}
	if a.DecidedAt != "" {
		fmt.Fprintf(w, "Decided at:  %s\n", a.DecidedAt)
	}
	fmt.Fprintf(w, "Created:     %s\n", a.CreatedAt)
}
