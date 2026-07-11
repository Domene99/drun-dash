package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/clitoken"
)

// listApprovalsHandler mocks GET /approvals, capturing the Authorization header
// and raw query string so tests can assert the Bearer token and pagination
// pass-through.
func listApprovalsHandler(t *testing.T, gotAuth, gotQuery *string, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/approvals" {
			t.Errorf("path = %s, want /approvals", r.URL.Path)
		}
		*gotAuth = r.Header.Get("Authorization")
		*gotQuery = r.URL.RawQuery
		respond(w, r)
	})
}

// decideApprovalHandler mocks POST /approvals/{approvalId}/decision, capturing
// the Authorization header, path, and decoded body.
func decideApprovalHandler(t *testing.T, gotAuth, gotPath *string, gotReq *approvalDecision, respond func(w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		*gotAuth = r.Header.Get("Authorization")
		*gotPath = r.URL.Path
		if gotReq != nil {
			if err := json.NewDecoder(r.Body).Decode(gotReq); err != nil {
				t.Errorf("decode request body: %v", err)
			}
		}
		respond(w, r)
	})
}

func TestApprovalsListSendsTokenAndParams(t *testing.T) {
	var gotAuth, gotQuery string
	withAuthClient(t, mockClient(listApprovalsHandler(t, &gotAuth, &gotQuery, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, approvalList{
			Items: []approval{
				{ID: "apr_1", SessionID: "ses_1", Action: "spend_over_cap", Status: "pending", CreatedAt: "2026-07-11T00:00:00Z"},
				{ID: "apr_2", SessionID: "ses_2", Action: "tool_call", Status: "approved", DecidedBy: "alice", CreatedAt: "2026-07-10T00:00:00Z"},
			},
			NextCursor: "cur_next",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "list-tok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "list", "--limit", "5", "--cursor", "cur_prev"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer list-tok" {
		t.Errorf("Authorization = %q, want 'Bearer list-tok'", gotAuth)
	}
	if !strings.Contains(gotQuery, "limit=5") || !strings.Contains(gotQuery, "cursor=cur_prev") {
		t.Errorf("query = %q, want limit=5 and cursor=cur_prev", gotQuery)
	}
	// Items are printed with their id and status; pending is highlighted.
	for _, want := range []string{"apr_1", "apr_2", "pending", "approved", ">"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
	// next_cursor yields a paging hint.
	if !strings.Contains(out.String(), "cur_next") {
		t.Errorf("stdout %q missing next_cursor hint", out.String())
	}
}

func TestApprovalsListEmpty(t *testing.T) {
	var gotAuth, gotQuery string
	withAuthClient(t, mockClient(listApprovalsHandler(t, &gotAuth, &gotQuery, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, approvalList{})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	if code := Run([]string{"approvals", "list"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No approvals found") {
		t.Errorf("stdout = %q, want an empty-list message", out.String())
	}
}

func TestApprovalsListNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "list"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

// No stored token => no request is sent: the not-logged-in path exits before
// touching the HTTP client.
func TestApprovalsListNoRequestWhenNotLoggedIn(t *testing.T) {
	var called bool
	withAuthClient(t, mockClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})))
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	if code := Run([]string{"approvals", "list"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if called {
		t.Errorf("HTTP request was sent despite no stored token")
	}
}

func TestApprovalsDecideApprove(t *testing.T) {
	var gotAuth, gotPath string
	var gotReq approvalDecision
	withAuthClient(t, mockClient(decideApprovalHandler(t, &gotAuth, &gotPath, &gotReq, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, approval{
			ID: "apr_1", SessionID: "ses_1", Action: "spend_over_cap",
			Status: "approved", DecidedBy: "reviewer", DecidedAt: "2026-07-11T01:00:00Z",
			CreatedAt: "2026-07-11T00:00:00Z",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "dec-tok", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "decide", "apr_1", "--decision", "approve", "--reason", "looks fine"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotAuth != "Bearer dec-tok" {
		t.Errorf("Authorization = %q, want 'Bearer dec-tok'", gotAuth)
	}
	if gotPath != "/approvals/apr_1/decision" {
		t.Errorf("path = %q, want /approvals/apr_1/decision", gotPath)
	}
	if gotReq.Decision != "approve" {
		t.Errorf("decision = %q, want approve", gotReq.Decision)
	}
	if gotReq.Reason != "looks fine" {
		t.Errorf("reason = %q, want 'looks fine'", gotReq.Reason)
	}
	if !strings.Contains(out.String(), "apr_1") || !strings.Contains(out.String(), "approved") {
		t.Errorf("stdout = %q, want the decided approval detail", out.String())
	}
}

// Flags may also precede the <approvalId> — the interspersed parse accepts the
// positional in any position, not only the documented id-first form.
func TestApprovalsDecideFlagsBeforeID(t *testing.T) {
	var gotAuth, gotPath string
	var gotReq approvalDecision
	withAuthClient(t, mockClient(decideApprovalHandler(t, &gotAuth, &gotPath, &gotReq, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, approval{ID: "apr_7", SessionID: "ses_7", Action: "x", Status: "approved", CreatedAt: "2026-07-11T00:00:00Z"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "decide", "--decision", "approve", "apr_7"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotPath != "/approvals/apr_7/decision" {
		t.Errorf("path = %q, want /approvals/apr_7/decision", gotPath)
	}
	if gotReq.Decision != "approve" {
		t.Errorf("decision = %q, want approve", gotReq.Decision)
	}
}

func TestApprovalsDecideDeny(t *testing.T) {
	var gotAuth, gotPath string
	var gotReq approvalDecision
	withAuthClient(t, mockClient(decideApprovalHandler(t, &gotAuth, &gotPath, &gotReq, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, approval{
			ID: "apr_9", SessionID: "ses_9", Action: "tool_call",
			Status: "denied", DecidedBy: "reviewer", CreatedAt: "2026-07-11T00:00:00Z",
		})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "decide", "apr_9", "--decision", "deny"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if gotReq.Decision != "deny" {
		t.Errorf("decision = %q, want deny", gotReq.Decision)
	}
	// Omitted --reason must not be sent.
	if gotReq.Reason != "" {
		t.Errorf("reason = %q, want empty when --reason omitted", gotReq.Reason)
	}
	if !strings.Contains(out.String(), "denied") {
		t.Errorf("stdout = %q, want denied status", out.String())
	}
}

// A 409 (already decided) prints a clear message and exits non-zero.
func TestApprovalsDecideAlreadyDecided(t *testing.T) {
	var gotAuth, gotPath string
	withAuthClient(t, mockClient(decideApprovalHandler(t, &gotAuth, &gotPath, nil, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, apiError{Code: "conflict", Message: "approval already denied"})
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "decide", "apr_1", "--decision", "approve"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(strings.ToLower(errOut.String()), "already been decided") {
		t.Errorf("stderr = %q, want an 'already been decided' message", errOut.String())
	}
	if !strings.Contains(errOut.String(), "approval already denied") {
		t.Errorf("stderr = %q, want the server's message", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failure", out.String())
	}
}

// An invalid --decision is a usage error caught before any request is sent.
func TestApprovalsDecideInvalidDecision(t *testing.T) {
	var called bool
	withAuthClient(t, mockClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "decide", "apr_1", "--decision", "maybe"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--decision") {
		t.Errorf("stderr = %q, want a --decision validation message", errOut.String())
	}
	if called {
		t.Errorf("HTTP request was sent despite an invalid --decision")
	}
}

// A missing --decision is likewise a usage error, no request sent.
func TestApprovalsDecideMissingDecision(t *testing.T) {
	var called bool
	withAuthClient(t, mockClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})))

	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	if code := Run([]string{"approvals", "decide", "apr_1"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if called {
		t.Errorf("HTTP request was sent despite a missing --decision")
	}
}

func TestApprovalsDecideMissingArg(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv(clitoken.ConfigDirEnv, cfgDir)
	seedToken(t, cfgDir, "tk", mockBase)

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "decide", "--decision", "approve"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "approvalId") {
		t.Errorf("stderr = %q, want an <approvalId> required message", errOut.String())
	}
}

func TestApprovalsDecideNotLoggedIn(t *testing.T) {
	t.Setenv(clitoken.ConfigDirEnv, t.TempDir())

	var out, errOut bytes.Buffer
	code := Run([]string{"approvals", "decide", "apr_1", "--decision", "approve"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Not logged in") {
		t.Errorf("stderr = %q, want 'Not logged in'", errOut.String())
	}
}

// The `approvals` command is registered in the top-level help and has its own
// subcommand usage.
func TestApprovalsRegisteredInTopLevelHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "approvals") {
		t.Errorf("top-level help %q missing 'approvals'", out.String())
	}
}

func TestApprovalsUsageListsSubcommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"approvals"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{"list", "decide"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("approvals usage %q missing %q", errOut.String(), want)
		}
	}
}
