package memberui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// writeFileTool is a real drun tool that this session's policy gates out (not
// offered to the model) — the escalatable, resumable case.
func writeFileTool() drunmcp.Tool {
	return drunmcp.Tool{
		Name:        "session_write_file",
		Description: "Write a file",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"}},"required":["session_id","path","content"]}`),
	}
}

// scopedSession stores a launched session whose bundle offers only session_bash,
// so session_write_file is policy-gated (a block the console can escalate).
func scopedSession() *launchedSession {
	ls := storedSession("http://proxy")
	ls.policyBundle = json.RawMessage(`{"allowed_tools":["session_bash"]}`)
	return ls
}

// cpApprovals is a mock control plane that raises an approval (POST /approvals)
// and reports it with the given terminal status when listed (GET /approvals).
func cpApprovals(decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/trajectory"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(IngestAck{Accepted: 1})
		case r.URL.Path == "/approvals" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Approval{ID: "appr-1", SessionID: "sess-1", Action: "session_write_file", Status: approvalPending})
		case r.URL.Path == "/approvals" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(ApprovalList{Items: []Approval{{ID: "appr-1", Status: decision}}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
}

// blockedProxyHandler first proposes a policy-gated tool call, then (after the
// escalation resolves) returns a final answer.
func blockedProxyHandler() http.HandlerFunc {
	var n int
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n++
		if n == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"session_write_file","arguments":"{\"path\":\"x\",\"content\":\"y\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}
}

func (f *fakeDrunConn) calledTool(name string) bool {
	for _, c := range f.calls {
		if c.name == name {
			return true
		}
	}
	return false
}

func TestEscalationApprovedResumesTool(t *testing.T) {
	conn := &fakeDrunConn{
		tools:  []drunmcp.Tool{bashTool(), writeFileTool()},
		callFn: func(string, any) (*drunmcp.CallToolResult, error) { return textResult("written"), nil },
	}
	s := newRunServerRT(t, cpApprovals(approvalApproved), conn,
		WithProxyHTTPClient(handlerClient(blockedProxyHandler())),
		WithApprovalPollInterval(time.Millisecond), WithApprovalWaitTimeout(2*time.Second))
	s.storeSession(scopedSession())

	authGet(s, "/sessions/sess-1")
	authPostForm(s, "/sessions/sess-1/turn", url.Values{"directive": {"write a file"}})
	rr := authGet(s, "/sessions/sess-1/stream")

	body := rr.Body.String()
	if !strings.Contains(body, "waiting for approval") {
		t.Errorf("stream should show the pending escalation: %q", body)
	}
	if !conn.calledTool("session_write_file") {
		t.Fatal("an approved escalation must resume and execute the blocked tool")
	}
	if !strings.Contains(body, "done") {
		t.Errorf("the turn should finish after the approved resume: %q", body)
	}
}

func TestEscalationDeniedFeedsGovernedDenial(t *testing.T) {
	conn := &fakeDrunConn{
		tools:  []drunmcp.Tool{bashTool(), writeFileTool()},
		callFn: func(string, any) (*drunmcp.CallToolResult, error) { return textResult("written"), nil },
	}
	s := newRunServerRT(t, cpApprovals(approvalDenied), conn,
		WithProxyHTTPClient(handlerClient(blockedProxyHandler())),
		WithApprovalPollInterval(time.Millisecond), WithApprovalWaitTimeout(time.Second))
	s.storeSession(scopedSession())

	authGet(s, "/sessions/sess-1")
	authPostForm(s, "/sessions/sess-1/turn", url.Values{"directive": {"write a file"}})
	rr := authGet(s, "/sessions/sess-1/stream")

	body := rr.Body.String()
	if !strings.Contains(body, "waiting for approval") {
		t.Errorf("stream should show the pending escalation: %q", body)
	}
	if conn.calledTool("session_write_file") {
		t.Fatal("a denied escalation must never execute the blocked tool")
	}
	if !strings.Contains(body, "done") {
		t.Errorf("the model should adapt and finish after a governed denial: %q", body)
	}
}

func TestGetApprovalStatusPagesAndFilters(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(ApprovalList{Items: []Approval{{ID: "a1", Status: approvalPending}}, NextCursor: "c2"})
			return
		}
		_ = json.NewEncoder(w).Encode(ApprovalList{Items: []Approval{{ID: "a2", Status: approvalApproved}}})
	})
	client := NewClient("http://cp", handlerClient(handler))

	got, err := client.GetApprovalStatus(context.Background(), "tok", "a2")
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if got != approvalApproved {
		t.Errorf("status = %q, want approved (found across pages)", got)
	}

	missing, err := client.GetApprovalStatus(context.Background(), "tok", "does-not-exist")
	if err != nil {
		t.Fatalf("GetApprovalStatus(missing): %v", err)
	}
	if missing != "" {
		t.Errorf("status = %q, want empty for an unknown approval", missing)
	}
}
