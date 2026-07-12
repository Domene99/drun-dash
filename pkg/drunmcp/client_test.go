package drunmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capturedReq records what the mock server saw for one request.
type capturedReq struct {
	method  string
	headers http.Header
	req     jsonRPCRequest
}

// newServer starts a mock drun-mcp endpoint. respond is invoked for each
// request with the decoded JSON-RPC message; it must write the HTTP response.
func newServer(t *testing.T, respond func(w http.ResponseWriter, req jsonRPCRequest)) (string, *[]capturedReq) {
	t.Helper()
	var mu sync.Mutex
	captured := &[]capturedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req jsonRPCRequest
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		*captured = append(*captured, capturedReq{method: r.Method, headers: r.Header.Clone(), req: req})
		mu.Unlock()
		respond(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, captured
}

func writeJSONResult(w http.ResponseWriter, id *int64, resultJSON string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, deref(id), resultJSON)
}

func sseFrame(data string) string { return "event: message\ndata: " + data + "\n\n" }

func writeSSE(w http.ResponseWriter, contentType string, frames ...string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		io.WriteString(w, f)
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// handleHandshake answers initialize and the initialized notification so tests
// that call Initialize can focus on later calls. Returns true if handled.
func handleHandshake(w http.ResponseWriter, req jsonRPCRequest) bool {
	switch req.Method {
	case "initialize":
		writeJSONResult(w, req.ID, `{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"drun-mcp","version":"1.2.3"}}`)
		return true
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	return false
}

func toolCall(t *testing.T, req jsonRPCRequest) (string, map[string]any) {
	t.Helper()
	params, ok := req.Params.(map[string]any)
	if !ok {
		t.Fatalf("tools/call params not an object: %T", req.Params)
	}
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	return name, args
}

// --- Transport-level behavior -------------------------------------------------

func TestAcceptHeaderOnEveryRequest(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		if handleHandshake(w, req) {
			return
		}
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"ok"}]}`)
	})

	c := NewClient(url)
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "some_tool", map[string]any{"x": 1}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if len(*captured) < 3 {
		t.Fatalf("expected >=3 requests (initialize, initialized, tools/call), got %d", len(*captured))
	}
	for i, cr := range *captured {
		if got := cr.headers.Get("Accept"); got != AcceptHeader {
			t.Errorf("request %d (%s): Accept = %q, want %q", i, cr.req.Method, got, AcceptHeader)
		}
		if got := cr.headers.Get("Content-Type"); got != "application/json" {
			t.Errorf("request %d (%s): Content-Type = %q, want application/json", i, cr.req.Method, got)
		}
		if cr.method != http.MethodPost {
			t.Errorf("request %d: method = %s, want POST", i, cr.method)
		}
	}
}

func TestInitializeSendsInitializedNotification(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		handleHandshake(w, req)
	})

	res, err := NewClient(url).Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if res.ServerInfo.Name != "drun-mcp" || res.ProtocolVersion != "2025-06-18" {
		t.Errorf("unexpected InitializeResult: %+v", res)
	}

	var methods []string
	for _, cr := range *captured {
		methods = append(methods, cr.req.Method)
	}
	want := []string{"initialize", "notifications/initialized"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Errorf("methods = %v, want %v", methods, want)
	}
	// The initialize request must not carry a protocol header (none negotiated
	// yet); the notification that follows must.
	if got := (*captured)[0].headers.Get("MCP-Protocol-Version"); got != "" {
		t.Errorf("initialize carried MCP-Protocol-Version = %q, want empty", got)
	}
	if got := (*captured)[1].headers.Get("MCP-Protocol-Version"); got != "2025-06-18" {
		t.Errorf("post-init request MCP-Protocol-Version = %q, want 2025-06-18", got)
	}
}

func TestCallToolJSONResponse(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"hello"}],"isError":false}`)
	})

	res, err := NewClient(url).CallTool(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "hello" {
		t.Errorf("Text() = %q, want hello", res.Text())
	}
}

func TestCallToolSSEResponse(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		// A log frame with a different id, then the real response.
		writeSSE(w, "text/event-stream",
			sseFrame(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`),
			sseFrame(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"streamed"}]}}`, deref(req.ID))),
		)
	})

	res, err := NewClient(url).CallTool(context.Background(), "streamy", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "streamed" {
		t.Errorf("Text() = %q, want streamed", res.Text())
	}
}

func TestSSEContentTypeWithCharset(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeSSE(w, "text/event-stream; charset=utf-8",
			sseFrame(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"ok"}]}}`, deref(req.ID))),
		)
	})

	res, err := NewClient(url).CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "ok" {
		t.Errorf("Text() = %q, want ok", res.Text())
	}
}

func TestSSESkipsUnmatchedIDs(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeSSE(w, "text/event-stream",
			sseFrame(`{"jsonrpc":"2.0","id":999,"result":{"content":[{"type":"text","text":"wrong"}]}}`),
			sseFrame("not json at all"),
			sseFrame(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"right"}]}}`, deref(req.ID))),
		)
	})

	res, err := NewClient(url).CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "right" {
		t.Errorf("Text() = %q, want right", res.Text())
	}
}

func Test406NotAcceptable(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		w.WriteHeader(http.StatusNotAcceptable)
	})

	_, err := NewClient(url).CallTool(context.Background(), "t", nil)
	if err == nil {
		t.Fatal("expected error on 406, got nil")
	}
	if !strings.Contains(err.Error(), "406") || !strings.Contains(err.Error(), "text/event-stream") {
		t.Errorf("406 error not actionable: %v", err)
	}
}

func Test404NotFound(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := NewClient(url).CallTool(context.Background(), "t", nil)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "/mcp") {
		t.Errorf("404 error not actionable: %v", err)
	}
}

func TestRPCErrorPropagates(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"bad params"}}`, deref(req.ID))
	})

	_, err := NewClient(url).CallTool(context.Background(), "t", nil)
	var rpcErr *RPCError
	if err == nil {
		t.Fatal("expected RPC error, got nil")
	}
	if !strings.Contains(err.Error(), "-32602") || !strings.Contains(err.Error(), "bad params") {
		t.Errorf("unexpected error: %v", err)
	}
	// Confirm the concrete type is preserved for callers.
	if ok := asRPCError(err, &rpcErr); !ok || rpcErr.Code != -32602 {
		t.Errorf("error not an *RPCError with code -32602: %v", err)
	}
}

func asRPCError(err error, target **RPCError) bool {
	e, ok := err.(*RPCError)
	if ok {
		*target = e
	}
	return ok
}

func TestMcpSessionIDRoundTrip(t *testing.T) {
	const sid = "sess-xyz"
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", sid)
		}
		if handleHandshake(w, req) {
			return
		}
		writeJSONResult(w, req.ID, `{"content":[]}`)
	})

	c := NewClient(url)
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "t", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// Every request after the initialize response must echo the session id.
	for i, cr := range *captured {
		if i == 0 {
			continue // initialize itself sends none
		}
		if got := cr.headers.Get("Mcp-Session-Id"); got != sid {
			t.Errorf("request %d (%s): Mcp-Session-Id = %q, want %q", i, cr.req.Method, got, sid)
		}
	}
}

// --- Session helper encoding --------------------------------------------------

func TestListTools(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"tools":[{"name":"session_bash","description":"run"},{"name":"create_session"}]}`)
	})

	res, err := NewClient(url).ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 2 || res.Tools[0].Name != "session_bash" {
		t.Errorf("unexpected tools: %+v", res.Tools)
	}
	if (*captured)[0].req.Method != "tools/list" {
		t.Errorf("method = %s, want tools/list", (*captured)[0].req.Method)
	}
}

func TestCreateSessionDecodesFromText(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		// No structuredContent; session_id arrives as JSON text (drun-mcp style).
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"session_id\":\"abc-123\"}"}]}`)
	})

	sid, err := NewClient(url).CreateSession(context.Background())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sid != "abc-123" {
		t.Errorf("session id = %q, want abc-123", sid)
	}
	name, _ := toolCall(t, (*captured)[0].req)
	if name != "create_session" {
		t.Errorf("tool name = %q, want create_session", name)
	}
}

func TestSessionBashEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"checkpoint_id\":5}"}]}`)
	})

	if _, err := NewClient(url).SessionBash(context.Background(), "s1", "ls -la"); err != nil {
		t.Fatalf("SessionBash: %v", err)
	}
	name, args := toolCall(t, (*captured)[0].req)
	if name != "session_bash" {
		t.Errorf("tool name = %q, want session_bash", name)
	}
	if args["session_id"] != "s1" || args["command"] != "ls -la" {
		t.Errorf("args = %v, want session_id=s1 command='ls -la'", args)
	}
}

func TestSessionForkCheckpointPrecedence(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"structuredContent":{"session_id":"forked"},"content":[]}`)
	})

	// Both id and label set: label must win.
	sid, err := NewClient(url).SessionFork(context.Background(), "s1", CheckpointRef{ID: intPtr(3), Label: "release"})
	if err != nil {
		t.Fatalf("SessionFork: %v", err)
	}
	if sid != "forked" {
		t.Errorf("forked session id = %q, want forked", sid)
	}
	_, args := toolCall(t, (*captured)[0].req)
	if _, hasID := args["checkpoint_id"]; hasID {
		t.Errorf("checkpoint_id should be omitted when label is set: %v", args)
	}
	if args["checkpoint_label"] != "release" {
		t.Errorf("checkpoint_label = %v, want release", args["checkpoint_label"])
	}
}

func TestSessionForkDefaultCheckpoint(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"session_id\":\"f\"}"}]}`)
	})

	if _, err := NewClient(url).SessionFork(context.Background(), "s1", CheckpointRef{}); err != nil {
		t.Fatalf("SessionFork: %v", err)
	}
	_, args := toolCall(t, (*captured)[0].req)
	if len(args) != 1 || args["session_id"] != "s1" {
		t.Errorf("default fork should send only session_id, got %v", args)
	}
}

func TestSessionDiffEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"--- a\n+++ b\n"}]}`)
	})

	diff, err := NewClient(url).SessionDiff(context.Background(), "s1", AtCheckpoint(0), AtLabel("head"))
	if err != nil {
		t.Fatalf("SessionDiff: %v", err)
	}
	if !strings.Contains(diff, "+++ b") {
		t.Errorf("diff text = %q", diff)
	}
	_, args := toolCall(t, (*captured)[0].req)
	if args["from_checkpoint_id"] != float64(0) {
		t.Errorf("from_checkpoint_id = %v, want 0", args["from_checkpoint_id"])
	}
	if args["to_checkpoint_label"] != "head" {
		t.Errorf("to_checkpoint_label = %v, want head", args["to_checkpoint_label"])
	}
}

func TestSessionCommitKeys(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[]}`)
	})

	if _, err := NewClient(url).SessionCommit(context.Background(), "s1", "a.go", "b.go"); err != nil {
		t.Fatalf("SessionCommit: %v", err)
	}
	_, args := toolCall(t, (*captured)[0].req)
	keys, ok := args["keys"].([]any)
	if !ok || len(keys) != 2 || keys[0] != "a.go" {
		t.Errorf("keys = %v, want [a.go b.go]", args["keys"])
	}
}

func TestToolErrorSurfacedAsError(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"isError":true,"content":[{"type":"text","text":"no such session"}]}`)
	})

	err := NewClient(url).SessionClose(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "no such session") {
		t.Errorf("expected tool error surfaced, got %v", err)
	}
}

// --- File, checkpoint, and history/tree helper encoding -----------------------

func TestSessionReadFileEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"package main\n"}]}`)
	})

	got, err := NewClient(url).SessionReadFile(context.Background(), "s1", "main.go")
	if err != nil {
		t.Fatalf("SessionReadFile: %v", err)
	}
	if got != "package main\n" {
		t.Errorf("content = %q, want %q", got, "package main\n")
	}
	name, args := toolCall(t, (*captured)[0].req)
	if name != "session_read_file" {
		t.Errorf("tool name = %q, want session_read_file", name)
	}
	if len(args) != 2 || args["session_id"] != "s1" || args["path"] != "main.go" {
		t.Errorf("args = %v, want session_id=s1 path=main.go", args)
	}
}

func TestSessionReadFileToolError(t *testing.T) {
	url, _ := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"isError":true,"content":[{"type":"text","text":"no such file"}]}`)
	})

	_, err := NewClient(url).SessionReadFile(context.Background(), "s1", "missing")
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("expected tool error surfaced, got %v", err)
	}
}

func TestSessionWriteFileEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"checkpoint_id\":2}"}]}`)
	})

	if _, err := NewClient(url).SessionWriteFile(context.Background(), "s1", "a/b.txt", "hello"); err != nil {
		t.Fatalf("SessionWriteFile: %v", err)
	}
	name, args := toolCall(t, (*captured)[0].req)
	if name != "session_write_file" {
		t.Errorf("tool name = %q, want session_write_file", name)
	}
	if len(args) != 3 || args["session_id"] != "s1" || args["path"] != "a/b.txt" || args["content"] != "hello" {
		t.Errorf("args = %v, want session_id=s1 path=a/b.txt content=hello", args)
	}
}

func TestSessionCheckpointEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"checkpoint_id\":7,\"label\":\"baseline\"}"}]}`)
	})

	got, err := NewClient(url).SessionCheckpoint(context.Background(), "s1", "baseline")
	if err != nil {
		t.Fatalf("SessionCheckpoint: %v", err)
	}
	if !strings.Contains(got, "baseline") {
		t.Errorf("payload = %q, want to contain baseline", got)
	}
	name, args := toolCall(t, (*captured)[0].req)
	if name != "session_checkpoint_label" {
		t.Errorf("tool name = %q, want session_checkpoint_label", name)
	}
	if args["session_id"] != "s1" || args["label"] != "baseline" {
		t.Errorf("args = %v, want session_id=s1 label=baseline", args)
	}
	// The current checkpoint is labeled, so no checkpoint_id is sent.
	if _, hasCP := args["checkpoint_id"]; hasCP {
		t.Errorf("checkpoint_id should be omitted (labels the current checkpoint): %v", args)
	}
}

func TestSessionHistoryEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"checkpoints\":[]}"}]}`)
	})

	if _, err := NewClient(url).SessionHistory(context.Background(), "s1"); err != nil {
		t.Fatalf("SessionHistory: %v", err)
	}
	name, args := toolCall(t, (*captured)[0].req)
	if name != "session_history" {
		t.Errorf("tool name = %q, want session_history", name)
	}
	if len(args) != 1 || args["session_id"] != "s1" {
		t.Errorf("args = %v, want only session_id=s1", args)
	}
}

func TestSessionTreeEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"sessions\":[]}"}]}`)
	})

	if _, err := NewClient(url).SessionTree(context.Background(), "s1"); err != nil {
		t.Fatalf("SessionTree: %v", err)
	}
	name, args := toolCall(t, (*captured)[0].req)
	if name != "session_tree" {
		t.Errorf("tool name = %q, want session_tree", name)
	}
	// session_tree is a global view: it takes no arguments. In particular the
	// sessionID accepted for API symmetry must not be forwarded.
	if _, has := args["session_id"]; has {
		t.Errorf("session_tree must not send session_id: %v", args)
	}
	if len(args) != 0 {
		t.Errorf("session_tree must send no arguments, got %v", args)
	}
}

func TestSessionSnapshotEncoding(t *testing.T) {
	url, captured := newServer(t, func(w http.ResponseWriter, req jsonRPCRequest) {
		writeJSONResult(w, req.ID, `{"content":[{"type":"text","text":"{\"path\":\"/tmp/s1.drun\"}"}]}`)
	})

	res, err := NewClient(url).SessionSnapshot(context.Background(), "s1")
	if err != nil {
		t.Fatalf("SessionSnapshot: %v", err)
	}
	if !strings.Contains(res.Text(), ".drun") {
		t.Errorf("snapshot payload = %q, want to contain .drun", res.Text())
	}
	name, args := toolCall(t, (*captured)[0].req)
	if name != "session_snapshot" {
		t.Errorf("tool name = %q, want session_snapshot", name)
	}
	if len(args) != 1 || args["session_id"] != "s1" {
		t.Errorf("args = %v, want only session_id=s1", args)
	}
}

func intPtr(i int) *int { return &i }
