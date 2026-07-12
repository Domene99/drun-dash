package memberui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	CallTool(ctx context.Context, name string, args any) (*drunmcp.CallToolResult, error)
	SessionClose(ctx context.Context, sessionID string) error
}

// drunDialer opens a [drunConn] at endpoint. The default dials a real
// *drunmcp.Client; tests swap in a fake through [WithDrunDialer].
type drunDialer func(endpoint string) drunConn

// defaultDrunDialer returns a live drun-mcp client for endpoint.
func defaultDrunDialer(endpoint string) drunConn {
	return drunmcp.NewClient(endpoint, drunmcp.WithClientInfo("drun-dash-member-console", "0.1.0"))
}

// managementTools are drun lifecycle tools the console owns and therefore never
// offers to the model: letting the model create, close, or restore sessions
// would let it escape or tear down the session the console is governing. The
// engine still enforces policy, but session lifecycle is a console concern, not
// an agent action (ADR-0004 §3).
var managementTools = map[string]bool{
	"create_session":  true,
	"session_close":   true,
	"session_restore": true,
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
// has no /health), creates a sandbox session, and prepares the tool set from
// ListTools intersected with the policy bundle's allowed_tools (ADR-0004 §3–§4).
// Any failure is fail-closed: the caller shows a clear error and refuses to run
// a turn rather than degrading to anything ungoverned.
func bindEngine(ctx context.Context, dial drunDialer, endpoint string, bundle json.RawMessage) (*boundEngine, error) {
	conn := dial(endpoint)
	if _, err := conn.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("memberui: drun engine unreachable at %s: %w", endpoint, err)
	}
	sessionID, err := conn.CreateSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("memberui: create drun session: %w", err)
	}
	listed, err := conn.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("memberui: list drun tools: %w", err)
	}
	allowed := parseAllowedTools(bundle)
	specs, wantsSession := buildToolSet(listed.Tools, allowed)
	return &boundEngine{
		conn:         conn,
		sessionID:    sessionID,
		endpoint:     endpoint,
		tools:        specs,
		executor:     &toolExecutor{conn: conn, sessionID: sessionID, wantsSession: wantsSession},
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

// buildToolSet turns discovered drun tools into the model's tool menu: it drops
// the console-owned lifecycle tools and, when the bundle names an allowed_tools
// list, keeps only the intersection (ADR-0004 §4). It returns the specs plus a
// per-name record of which tools want the bound session_id injected at
// execution — that map is also the authoritative "is this tool offered?" set the
// executor blocks against. Specs are sorted by name for stable rendering.
func buildToolSet(tools []drunmcp.Tool, allowed map[string]bool) (specs []agentrun.ToolSpec, wantsSession map[string]bool) {
	wantsSession = make(map[string]bool, len(tools))
	for _, t := range tools {
		if managementTools[t.Name] {
			continue
		}
		if allowed != nil && !allowed[t.Name] {
			continue
		}
		params, ws := prepareToolSchema(t.InputSchema)
		specs = append(specs, agentrun.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
		wantsSession[t.Name] = ws
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, wantsSession
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
	wantsSession map[string]bool // offered tools -> inject session_id
}

// Execute runs one model-proposed tool call against the bound drun session.
func (e *toolExecutor) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	injectSession, offered := e.wantsSession[name]
	if !offered {
		// Outside the offered set: block so it escalates (ADR-0004 §4/§6)
		// rather than silently running an ungoverned tool.
		return "", fmt.Errorf("%w: tool %q is not in this session's allowed tool set", agentrun.ErrToolBlocked, name)
	}

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
