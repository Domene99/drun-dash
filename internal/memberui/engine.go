package memberui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Domene99/drun-dash/internal/agentrun"
	"github.com/Domene99/drun-dash/pkg/drunmcp"
	"github.com/Domene99/drun-dash/pkg/policy"
)

// drunConn is the slice of *drunmcp.Client the console uses to drive a local
// drun sandbox session (ADR-0004 §3–§4). Naming it as an interface lets the
// tests inject a fake engine in place of a live drun-mcp server, which the
// network-sandboxed test builder cannot reach.
type drunConn interface {
	Initialize(ctx context.Context) (*drunmcp.InitializeResult, error)
	ListTools(ctx context.Context) (*drunmcp.ListToolsResult, error)
	CreateSession(ctx context.Context) (string, error)
	// SessionMount copies a host working directory into the fresh sandbox
	// session so the agent works on real files and an explicit Commit can write
	// back to that folder (issue #296). Mounting is a console concern, driven at
	// bind time from the member's chosen path — never an agent tool call.
	SessionMount(ctx context.Context, sessionID, path string) (*drunmcp.CallToolResult, error)
	CallTool(ctx context.Context, name string, args any) (*drunmcp.CallToolResult, error)
	SessionClose(ctx context.Context, sessionID string) error

	// Console-driven checkpoint/diff/commit calls. These are member/console
	// operations (the checkpoint timeline, diff-before-commit) rather than
	// agent tool calls, so the console invokes them directly on the bound
	// session — never through the model's tool set (ADR-0004 §3, design §6.3).
	SessionHistory(ctx context.Context, sessionID string) (*drunmcp.CallToolResult, error)
	SessionTree(ctx context.Context, sessionID string) (*drunmcp.CallToolResult, error)
	SessionDiff(ctx context.Context, sessionID string, from, to drunmcp.CheckpointRef) (string, error)
	SessionCommit(ctx context.Context, sessionID string, keys ...string) (*drunmcp.CallToolResult, error)
	// SessionExport lists the session's newly-created (no host origin) files by
	// workspace-relative key, and SessionReadFile reads one back. session_commit
	// writes back only modified mounted files, so the console uses these to
	// persist files the agent *created* into the member's working directory
	// (issue #296 follow-up).
	SessionExport(ctx context.Context, sessionID string) ([]string, error)
	SessionReadFile(ctx context.Context, sessionID, path string) (string, error)
}

// drunDialer opens a [drunConn] at endpoint. The default dials a real
// *drunmcp.Client; tests swap in a fake through [WithDrunDialer].
type drunDialer func(endpoint string) drunConn

// defaultDrunDialer returns a live drun-mcp client for endpoint.
func defaultDrunDialer(endpoint string) drunConn {
	return drunmcp.NewClient(endpoint, drunmcp.WithClientInfo("drun-dash-member-console", "0.1.0"))
}

// managementTools are drun tools the console owns and therefore never offers to
// the model, for two reasons. First, session lifecycle (create/close/restore):
// letting the model create, close, or restore sessions would let it escape or
// tear down the session the console is governing. Second, and more importantly,
// the host-boundary writes (commit/export/snapshot): committing sandbox changes
// back to the member's host files must happen ONLY through the explicit
// diff-before-commit member action, never as an autonomous agent step — that is
// the core host-file-safety guarantee (design §6.3). The engine still enforces
// policy at execution, but these operations are console concerns, not agent
// actions (ADR-0004 §3).
var managementTools = map[string]bool{
	"create_session":  true,
	"session_close":   true,
	"session_restore": true,
	// Host-boundary writes: reachable only via explicit member action.
	"session_commit":   true,
	"session_export":   true,
	"session_snapshot": true,
}

// boundEngine is a live drun sandbox binding for one launched session: the
// sequential drun client, the created drun session id, and the tool set offered
// to the model (ADR-0004 §3–§4). A drunmcp.Client is sequential, so the run
// serializes all use of conn under its own lock (see run.go).
type boundEngine struct {
	conn         drunConn
	sessionID    string
	endpoint     string
	tools        []agentrun.ToolSpec
	executor     *toolExecutor
	policyScoped bool // tool set was intersected with the bundle's allowed_tools
}

// bindEngine dials the local drun engine, probes it with Initialize (drun-mcp
// has no /health), creates a sandbox session, optionally mounts the member's
// chosen host working directory into it, and prepares the tool set from
// ListTools intersected with the policy bundle's allowed_tools (ADR-0004 §3–§4).
// Any failure is fail-closed: the caller shows a clear error and refuses to run
// a turn rather than degrading to anything ungoverned.
func bindEngine(ctx context.Context, dial drunDialer, endpoint string, bundle json.RawMessage, workingDir string) (*boundEngine, error) {
	conn := dial(endpoint)
	if _, err := conn.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("memberui: drun engine unreachable at %s: %w", endpoint, err)
	}
	sessionID, err := conn.CreateSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("memberui: create drun session: %w", err)
	}
	// Mount the member's chosen host working directory into the fresh session so
	// the agent works on real files and the explicit diff-before-commit action
	// writes back to that folder (issue #296). With no dir the session stays a
	// scratch sandbox (the prior behavior). A mount failure is fail-closed: abort
	// the bind with a readable error rather than silently running against a temp
	// dir the member can never find.
	if dir := strings.TrimSpace(workingDir); dir != "" {
		if _, err := conn.SessionMount(ctx, sessionID, dir); err != nil {
			return nil, fmt.Errorf("could not mount %s: %w", dir, err)
		}
	}
	listed, err := conn.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("memberui: list drun tools: %w", err)
	}
	allowed := parseAllowedTools(bundle)
	specs, wantsSession, offered := buildToolSet(listed.Tools, allowed)
	return &boundEngine{
		conn:      conn,
		sessionID: sessionID,
		endpoint:  endpoint,
		tools:     specs,
		executor: &toolExecutor{
			conn:         conn,
			sessionID:    sessionID,
			offered:      offered,
			wantsSession: wantsSession,
		},
		policyScoped: allowed != nil,
	}, nil
}

// close terminates the bound drun session, freeing the sandbox subprocess. It is
// best-effort — a close failure is not worth surfacing to the member.
func (b *boundEngine) close(ctx context.Context) {
	if b == nil || b.conn == nil || b.sessionID == "" {
		return
	}
	_ = b.conn.SessionClose(ctx, b.sessionID)
}

// buildToolSet turns discovered drun tools into the model's tool menu. It drops
// the console-owned lifecycle/host-write tools entirely (they are never
// executable, even on approval), and when the bundle names an allowed_tools list
// it offers only the intersection to the model (ADR-0004 §4). It returns three
// things: the offered specs (sorted for stable rendering); wantsSession, a
// per-name record of which tools want the bound session_id injected — computed
// for EVERY executable (non-management) discovered tool, not just the offered
// ones, so an escalated-then-approved tool can be executed with the right args;
// and offered, the set of tools actually presented to the model, which is the
// authoritative "is this allowed without approval?" set the executor blocks
// against.
func buildToolSet(tools []drunmcp.Tool, allowed map[string]bool) (specs []agentrun.ToolSpec, wantsSession, offered map[string]bool) {
	wantsSession = make(map[string]bool, len(tools))
	offered = make(map[string]bool, len(tools))
	for _, t := range tools {
		if managementTools[t.Name] {
			continue
		}
		params, ws := prepareToolSchema(t.InputSchema)
		wantsSession[t.Name] = ws
		if allowed != nil && !allowed[t.Name] {
			// Discovered but policy-gated: not offered to the model. If an admin
			// approves an escalation for it, the executor can still run it.
			continue
		}
		offered[t.Name] = true
		specs = append(specs, agentrun.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, wantsSession, offered
}

// prepareToolSchema rewrites a drun tool's inputSchema into the JSON-Schema the
// model sees: it strips the session_id property (and its required entry) so the
// model never supplies it — the console injects the bound session id at
// execution — while preserving every other schema key verbatim. It reports
// whether the original schema declared a session_id, so the executor knows to
// inject it. A missing or unparseable schema yields a permissive object schema.
func prepareToolSchema(raw json.RawMessage) (params json.RawMessage, wantsSession bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{"type":"object"}`), false
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil {
		// Not an object schema we can rewrite; pass it through unchanged.
		return raw, false
	}

	if propsRaw, ok := schema["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(propsRaw, &props); err == nil {
			if _, has := props["session_id"]; has {
				wantsSession = true
				delete(props, "session_id")
				if b, err := json.Marshal(props); err == nil {
					schema["properties"] = b
				}
			}
		}
	}

	if reqRaw, ok := schema["required"]; ok {
		var req []string
		if err := json.Unmarshal(reqRaw, &req); err == nil {
			kept := make([]string, 0, len(req))
			for _, r := range req {
				if r != "session_id" {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(schema, "required")
			} else if b, err := json.Marshal(kept); err == nil {
				schema["required"] = b
			}
		}
	}

	if b, err := json.Marshal(schema); err == nil {
		return b, wantsSession
	}
	return raw, wantsSession
}

// parseAllowedTools best-effort extracts an allowed_tools list from the raw
// policy bundle, checking both the top level and drun_config. It returns nil
// when no such list is present — the signal to offer the full discovered drun
// tool set (the engine and proxy still enforce policy at execution; ADR-0004
// §4). This tolerant parse means a future bundle that *does* carry allowed_tools
// tightens the model's tool menu automatically, with no change here.
func parseAllowedTools(bundle json.RawMessage) map[string]bool {
	if len(bytes.TrimSpace(bundle)) == 0 {
		return nil
	}
	var doc struct {
		AllowedTools []string `json:"allowed_tools"`
		DrunConfig   struct {
			AllowedTools []string `json:"allowed_tools"`
		} `json:"drun_config"`
	}
	if err := json.Unmarshal(bundle, &doc); err != nil {
		return nil
	}
	list := doc.AllowedTools
	if len(list) == 0 {
		list = doc.DrunConfig.AllowedTools
	}
	set := make(map[string]bool, len(list))
	for _, name := range list {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// bundleVerifier verifies a launch-issued policy bundle fail-closed before any
// turn runs (ADR-0004 §3). It returns nil only when the bundle may be trusted.
type bundleVerifier func(bundle json.RawMessage, now time.Time) error

// defaultBundleVerifier runs the checks the console can perform without the
// org's signing key: parse (policy.Load), freshness, and content-hash integrity.
// Signature verification needs the org's Ed25519 public key, whose distribution
// is future work (pkg/policy, ADR-0003); until it is wired the console verifies
// structure, freshness, and integrity and refuses to run a turn on any failure —
// no verified bundle, no run. An empty bundle is treated as unverified.
func defaultBundleVerifier(bundle json.RawMessage, now time.Time) error {
	if len(bytes.TrimSpace(bundle)) == 0 {
		return errors.New("session has no policy bundle to verify")
	}
	b, err := policy.Load(bundle)
	if err != nil {
		return fmt.Errorf("policy bundle failed to load: %w", err)
	}
	if err := b.CheckFreshness(now); err != nil {
		return fmt.Errorf("policy bundle is not fresh: %w", err)
	}
	if err := b.VerifyContentHash(); err != nil {
		return fmt.Errorf("policy bundle content hash mismatch: %w", err)
	}
	return nil
}

// toolExecutor implements [agentrun.ToolExecutor] by dispatching each model tool
// call to the bound drun session over [drunConn] (ADR-0004 §4). It injects the
// bound drun session id into calls whose schema declared one — the model never
// sees or supplies it — and enforces the offered tool set fail-closed: a call to
// a tool outside the set is an [agentrun.ErrToolBlocked] escalation, not a hard
// error. A tool-level engine error (a failed command, a refusal) is fed back to
// the model as its result so the model can adapt, rather than aborting the turn.
type toolExecutor struct {
	conn         drunConn
	sessionID    string
	offered      map[string]bool // tools offered to the model (policy-allowed, no approval needed)
	wantsSession map[string]bool // every executable drun tool -> inject session_id

	mu       sync.Mutex
	approved map[string]bool // tools an admin approved this run, unblocked for resume (ADR-0004 §6)
}

// allow records that an escalation for name was approved, so the next Execute of
// name proceeds instead of blocking again. It is called by the escalation hook
// on an approved decision before the turn loop resumes the tool.
func (e *toolExecutor) allow(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.approved == nil {
		e.approved = make(map[string]bool)
	}
	e.approved[name] = true
}

// isApproved reports whether name was approved this run.
func (e *toolExecutor) isApproved(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.approved[name]
}

// Execute runs one model-proposed tool call against the bound drun session.
func (e *toolExecutor) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	// Console-owned lifecycle/host-write tools (create/close/restore, and
	// commit/export/snapshot) are never executable by the agent — not even on
	// approval. This is what keeps host-file commits off the agent loop (the
	// core diff-before-commit guarantee, design §6.3). They are dropped from the
	// tool menu, so a call here means the model went off-menu: block it.
	if managementTools[name] {
		return "", fmt.Errorf("%w: tool %q is reserved to the console", agentrun.ErrToolBlocked, name)
	}
	// Offered tools run immediately. A policy-gated tool runs only after an
	// admin approves its escalation (allow); until then, block so it escalates
	// (ADR-0004 §4/§6) rather than silently running ungoverned.
	if !e.offered[name] && !e.isApproved(name) {
		return "", fmt.Errorf("%w: tool %q is not in this session's allowed tool set", agentrun.ErrToolBlocked, name)
	}
	injectSession := e.wantsSession[name]

	argMap := map[string]any{}
	if trimmed := bytes.TrimSpace(args); len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &argMap); err != nil {
			return "", fmt.Errorf("memberui: tool %q arguments are not a JSON object: %w", name, err)
		}
	}
	if injectSession {
		argMap["session_id"] = e.sessionID
	}

	res, err := e.conn.CallTool(ctx, name, argMap)
	if err != nil {
		return "", fmt.Errorf("memberui: tool %q failed: %w", name, err)
	}
	return res.Text(), nil
}
