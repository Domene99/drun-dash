package memberui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/agentrun"
	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// fakeDrunConn is an in-memory drunConn for tests: it stands in for a live
// drun-mcp server (which the network-sandboxed builder cannot reach).
type fakeDrunConn struct {
	initErr   error
	createErr error
	sessionID string
	tools     []drunmcp.Tool

	callFn func(name string, args any) (*drunmcp.CallToolResult, error)
	calls  []fakeToolCall
	closed bool

	// Working-directory mount stubs (issue #296). mounted records every path the
	// bind mounted (in order); mountErr forces a mount failure so the bind aborts
	// with a readable error.
	mounted  []string
	mountErr error

	// Checkpoint-timeline / diff-before-commit stubs (design §6.3).
	historyText string
	treeText    string
	diffText    string
	diffErr     error
	commitErr   error
	committed   bool
	commitKeys  []string

	// New-file export stub: exportFiles (name→content) are the sandbox-created
	// files SessionExport simulates drun writing into the requested output_dir;
	// exportOutputDir records the output_dir it was called with.
	exportFiles     map[string]string
	exportErr       error
	exportOutputDir string
}

type fakeToolCall struct {
	name string
	args any
}

func (f *fakeDrunConn) Initialize(context.Context) (*drunmcp.InitializeResult, error) {
	if f.initErr != nil {
		return nil, f.initErr
	}
	return &drunmcp.InitializeResult{ProtocolVersion: "test"}, nil
}

func (f *fakeDrunConn) ListTools(context.Context) (*drunmcp.ListToolsResult, error) {
	return &drunmcp.ListToolsResult{Tools: f.tools}, nil
}

func (f *fakeDrunConn) CreateSession(context.Context) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.sessionID == "" {
		return "drun-sess", nil
	}
	return f.sessionID, nil
}

func (f *fakeDrunConn) SessionMount(_ context.Context, _ string, path string) (*drunmcp.CallToolResult, error) {
	if f.mountErr != nil {
		return nil, f.mountErr
	}
	f.mounted = append(f.mounted, path)
	return textResult("mounted"), nil
}

func (f *fakeDrunConn) CallTool(_ context.Context, name string, args any) (*drunmcp.CallToolResult, error) {
	f.calls = append(f.calls, fakeToolCall{name: name, args: args})
	if f.callFn != nil {
		return f.callFn(name, args)
	}
	return &drunmcp.CallToolResult{Content: []drunmcp.ContentBlock{{Type: "text", Text: "ok"}}}, nil
}

func (f *fakeDrunConn) SessionClose(context.Context, string) error {
	f.closed = true
	return nil
}

func textResult(text string) *drunmcp.CallToolResult {
	return &drunmcp.CallToolResult{Content: []drunmcp.ContentBlock{{Type: "text", Text: text}}}
}

func (f *fakeDrunConn) SessionHistory(context.Context, string) (*drunmcp.CallToolResult, error) {
	return textResult(f.historyText), nil
}

func (f *fakeDrunConn) SessionTree(context.Context, string) (*drunmcp.CallToolResult, error) {
	return textResult(f.treeText), nil
}

func (f *fakeDrunConn) SessionDiff(_ context.Context, _ string, _, _ drunmcp.CheckpointRef) (string, error) {
	if f.diffErr != nil {
		return "", f.diffErr
	}
	return f.diffText, nil
}

func (f *fakeDrunConn) SessionCommit(_ context.Context, _ string, keys ...string) (*drunmcp.CallToolResult, error) {
	if f.commitErr != nil {
		return nil, f.commitErr
	}
	f.committed = true
	f.commitKeys = keys
	return textResult("committed"), nil
}

func (f *fakeDrunConn) SessionExport(_ context.Context, _, outputDir string) ([]string, error) {
	if f.exportErr != nil {
		return nil, f.exportErr
	}
	f.exportOutputDir = outputDir
	// Simulate drun writing the exported files into output_dir.
	written := make([]string, 0, len(f.exportFiles))
	for name, content := range f.exportFiles {
		dest := filepath.Join(outputDir, name)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return written, err
		}
		written = append(written, dest)
	}
	return written, nil
}

// bashTool is a representative drun tool whose schema requires session_id.
func bashTool() drunmcp.Tool {
	return drunmcp.Tool{
		Name:        "session_bash",
		Description: "Run a shell command",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"},"command":{"type":"string"}},"required":["session_id","command"]}`),
	}
}

func TestPrepareToolSchemaStripsSessionID(t *testing.T) {
	params, wants := prepareToolSchema(bashTool().InputSchema)
	if !wants {
		t.Fatal("expected wantsSession=true for a schema declaring session_id")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("params not valid JSON: %v", err)
	}
	if _, ok := schema.Properties["session_id"]; ok {
		t.Error("session_id property should be stripped from the model schema")
	}
	if _, ok := schema.Properties["command"]; !ok {
		t.Error("command property should be preserved")
	}
	for _, r := range schema.Required {
		if r == "session_id" {
			t.Error("session_id should be removed from required")
		}
	}
}

func TestPrepareToolSchemaEmptyGivesObject(t *testing.T) {
	params, wants := prepareToolSchema(nil)
	if wants {
		t.Error("empty schema should not want a session_id")
	}
	if string(params) != `{"type":"object"}` {
		t.Errorf("params = %s, want a permissive object schema", params)
	}
}

func TestParseAllowedTools(t *testing.T) {
	if got := parseAllowedTools(nil); got != nil {
		t.Errorf("nil bundle => nil, got %v", got)
	}
	if got := parseAllowedTools(json.RawMessage(`{"drun_config":{}}`)); got != nil {
		t.Errorf("no allowed_tools => nil, got %v", got)
	}
	top := parseAllowedTools(json.RawMessage(`{"allowed_tools":["session_bash","session_read_file"]}`))
	if !top["session_bash"] || !top["session_read_file"] || len(top) != 2 {
		t.Errorf("top-level allowed_tools not parsed: %v", top)
	}
	nested := parseAllowedTools(json.RawMessage(`{"drun_config":{"allowed_tools":["only_this"]}}`))
	if !nested["only_this"] || len(nested) != 1 {
		t.Errorf("nested allowed_tools not parsed: %v", nested)
	}
}

func TestBuildToolSetDropsManagementAndIntersects(t *testing.T) {
	tools := []drunmcp.Tool{
		bashTool(),
		{Name: "session_read_file", InputSchema: json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"}}}`)},
		{Name: "create_session"}, // management: never offered
	}

	// No policy list: full set minus management tools.
	specs, wants, offered := buildToolSet(tools, nil)
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2 (management dropped)", len(specs))
	}
	if specs[0].Name != "session_bash" || specs[1].Name != "session_read_file" {
		t.Errorf("specs not sorted/expected: %+v", specs)
	}
	if !wants["session_bash"] {
		t.Error("session_bash should want a session_id injected")
	}
	if _, ok := wants["create_session"]; ok {
		t.Error("management tool should not appear in wantsSession")
	}
	if !offered["session_bash"] || !offered["session_read_file"] || len(offered) != 2 {
		t.Errorf("offered set = %v, want both non-management tools", offered)
	}

	// Policy list intersects.
	specs2, wants2, offered2 := buildToolSet(tools, map[string]bool{"session_bash": true})
	if len(specs2) != 1 || specs2[0].Name != "session_bash" {
		t.Errorf("intersection failed: %+v", specs2)
	}
	if offered2["session_read_file"] {
		t.Error("a policy-gated tool must not be offered to the model")
	}
	if !wants2["session_read_file"] {
		t.Error("a policy-gated tool must still be in wantsSession so an approved escalation can run it")
	}
}

func TestBuildToolSetWithholdsHostWrites(t *testing.T) {
	tools := []drunmcp.Tool{
		bashTool(),
		{Name: "session_commit"},
		{Name: "session_export"},
		{Name: "session_snapshot"},
	}
	specs, wants, offered := buildToolSet(tools, nil)
	if len(specs) != 1 || specs[0].Name != "session_bash" {
		t.Fatalf("host-write tools should be withheld from the model: %+v", specs)
	}
	for _, name := range []string{"session_commit", "session_export", "session_snapshot"} {
		if offered[name] || wants[name] {
			t.Errorf("%s must never be offered or executable by the agent", name)
		}
	}
}

func TestToolExecutorBlocksHostWriteEvenIfApproved(t *testing.T) {
	exec := &toolExecutor{conn: &fakeDrunConn{}, sessionID: "drun-sess", offered: map[string]bool{}, wantsSession: map[string]bool{}}
	// Even after an (erroneous) approval, a host-write/lifecycle tool is never
	// executable by the agent — the core diff-before-commit guarantee.
	exec.allow("session_commit")
	if _, err := exec.Execute(context.Background(), "session_commit", json.RawMessage(`{}`)); !errors.Is(err, agentrun.ErrToolBlocked) {
		t.Errorf("session_commit must stay blocked even if approved, got %v", err)
	}
}

func TestToolExecutorInjectsSessionAndBlocksUnknown(t *testing.T) {
	conn := &fakeDrunConn{callFn: func(name string, args any) (*drunmcp.CallToolResult, error) {
		m, _ := args.(map[string]any)
		if m["session_id"] != "drun-sess" {
			t.Errorf("session_id not injected: %v", m)
		}
		return &drunmcp.CallToolResult{Content: []drunmcp.ContentBlock{{Type: "text", Text: "listed"}}}, nil
	}}
	exec := &toolExecutor{conn: conn, sessionID: "drun-sess", offered: map[string]bool{"session_bash": true}, wantsSession: map[string]bool{"session_bash": true}}

	out, err := exec.Execute(context.Background(), "session_bash", json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "listed" {
		t.Errorf("out = %q, want listed", out)
	}

	// A tool outside the offered set is a policy block (escalatable), not a hard
	// error.
	_, err = exec.Execute(context.Background(), "session_close", json.RawMessage(`{}`))
	if !errors.Is(err, agentrun.ErrToolBlocked) {
		t.Errorf("err = %v, want ErrToolBlocked", err)
	}
}

func TestToolExecutorNoSessionInjectionWhenNotWanted(t *testing.T) {
	conn := &fakeDrunConn{callFn: func(name string, args any) (*drunmcp.CallToolResult, error) {
		m, _ := args.(map[string]any)
		if _, ok := m["session_id"]; ok {
			t.Errorf("session_id should not be injected for a tool that did not declare it: %v", m)
		}
		return &drunmcp.CallToolResult{}, nil
	}}
	exec := &toolExecutor{conn: conn, sessionID: "drun-sess", offered: map[string]bool{"session_tree": true}, wantsSession: map[string]bool{"session_tree": false}}
	if _, err := exec.Execute(context.Background(), "session_tree", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestDefaultBundleVerifierRejectsEmptyAndGarbage(t *testing.T) {
	if err := defaultBundleVerifier(nil, time.Now()); err == nil {
		t.Error("empty bundle should be rejected (no verified bundle, no run)")
	}
	if err := defaultBundleVerifier(json.RawMessage(`{"not":"a bundle"}`), time.Now()); err == nil {
		t.Error("a non-bundle document should fail to load")
	}
}
