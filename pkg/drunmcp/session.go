package drunmcp

import "context"

// This file provides typed helpers for the drun-mcp session lifecycle tools
// that the drun-dash CLI relies on. Each helper wraps CallTool with the exact
// argument names drun-mcp expects, so callers do not hand-build argument maps.
//
// Tool outputs whose shape is stable (a session_id) are decoded to Go values;
// helpers for tools with richer, evolving output return the raw
// *CallToolResult so callers can use Text() or Decode() as needed.

// CheckpointRef identifies a checkpoint by numeric id or label. When both are
// set, Label takes precedence, matching drun-mcp semantics.
type CheckpointRef struct {
	ID    *int
	Label string
}

// AtCheckpoint is a convenience constructor for a CheckpointRef by id.
func AtCheckpoint(id int) CheckpointRef { return CheckpointRef{ID: &id} }

// AtLabel is a convenience constructor for a CheckpointRef by label.
func AtLabel(label string) CheckpointRef { return CheckpointRef{Label: label} }

// isSet reports whether the ref points at a specific checkpoint.
func (r CheckpointRef) isSet() bool { return r.Label != "" || r.ID != nil }

// apply writes the ref into args under the given id/label keys.
func (r CheckpointRef) apply(args map[string]any, idKey, labelKey string) {
	switch {
	case r.Label != "":
		args[labelKey] = r.Label
	case r.ID != nil:
		args[idKey] = *r.ID
	}
}

// ensureOK returns the result unless the call failed or the tool itself
// reported an error, in which case it surfaces the tool's text as a Go error.
func ensureOK(r *CallToolResult, err error) (*CallToolResult, error) {
	if err != nil {
		return nil, err
	}
	if r.IsError {
		return r, &RPCError{Code: -1, Message: "tool error: " + r.Text()}
	}
	return r, nil
}

// sessionIDPayload is the common shape returned by tools that create a session.
type sessionIDPayload struct {
	SessionID string `json:"session_id"`
}

// CreateSession creates a new sandbox session and returns its id.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	res, err := ensureOK(c.CallTool(ctx, "create_session", map[string]any{}))
	if err != nil {
		return "", err
	}
	var out sessionIDPayload
	if err := res.Decode(&out); err != nil {
		return "", err
	}
	return out.SessionID, nil
}

// SessionClose terminates a session and frees its resources.
func (c *Client) SessionClose(ctx context.Context, sessionID string) error {
	_, err := ensureOK(c.CallTool(ctx, "session_close", map[string]any{
		"session_id": sessionID,
	}))
	return err
}

// SessionBash runs a shell command in the session workspace and returns the raw
// tool result (metadata JSON in the content; use CallToolResult.Decode to
// unmarshal, or read stdout/stderr via separate drun tools).
func (c *Client) SessionBash(ctx context.Context, sessionID, command string) (*CallToolResult, error) {
	return ensureOK(c.CallTool(ctx, "session_bash", map[string]any{
		"session_id": sessionID,
		"command":    command,
	}))
}

// SessionMount copies a host file or directory into the session.
func (c *Client) SessionMount(ctx context.Context, sessionID, path string) (*CallToolResult, error) {
	return ensureOK(c.CallTool(ctx, "session_mount", map[string]any{
		"session_id": sessionID,
		"path":       path,
	}))
}

// SessionFork branches a new session from an existing one. Pass a zero
// CheckpointRef to branch from the current checkpoint. Returns the new
// session id.
func (c *Client) SessionFork(ctx context.Context, sessionID string, from CheckpointRef) (string, error) {
	args := map[string]any{"session_id": sessionID}
	from.apply(args, "checkpoint_id", "checkpoint_label")
	res, err := ensureOK(c.CallTool(ctx, "session_fork", args))
	if err != nil {
		return "", err
	}
	var out sessionIDPayload
	if err := res.Decode(&out); err != nil {
		return "", err
	}
	return out.SessionID, nil
}

// SessionRollback moves the session head back to a prior checkpoint. This is
// destructive; see the drun-mcp tool docs. The target must be set.
func (c *Client) SessionRollback(ctx context.Context, sessionID string, to CheckpointRef) error {
	args := map[string]any{"session_id": sessionID}
	to.apply(args, "checkpoint_id", "checkpoint_label")
	_, err := ensureOK(c.CallTool(ctx, "session_rollback", args))
	return err
}

// SessionDiff returns a unified diff between two checkpoints. Pass zero
// CheckpointRefs to use drun-mcp's defaults (mounted state vs current head).
func (c *Client) SessionDiff(ctx context.Context, sessionID string, from, to CheckpointRef) (string, error) {
	args := map[string]any{"session_id": sessionID}
	from.apply(args, "from_checkpoint_id", "from_checkpoint_label")
	to.apply(args, "to_checkpoint_id", "to_checkpoint_label")
	res, err := ensureOK(c.CallTool(ctx, "session_diff", args))
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// SessionCommit writes changed mounted files back to their host paths. Pass
// specific keys to commit a subset, or none to commit all changed files.
func (c *Client) SessionCommit(ctx context.Context, sessionID string, keys ...string) (*CallToolResult, error) {
	args := map[string]any{"session_id": sessionID}
	if len(keys) > 0 {
		args["keys"] = keys
	}
	return ensureOK(c.CallTool(ctx, "session_commit", args))
}

// GetFetchAllowlist returns the raw result of get_fetch_allowlist (the list of
// domains permitted for outbound fetches). The output shape is server-defined;
// use CallToolResult.Text or Decode to interpret it.
func (c *Client) GetFetchAllowlist(ctx context.Context) (*CallToolResult, error) {
	return ensureOK(c.CallTool(ctx, "get_fetch_allowlist", map[string]any{}))
}

// SessionGetEnv reads a host environment variable exposed to the session by the
// server's env allowlist, returning its value as text.
func (c *Client) SessionGetEnv(ctx context.Context, sessionID, name string) (string, error) {
	res, err := ensureOK(c.CallTool(ctx, "session_get_env", map[string]any{
		"session_id": sessionID,
		"name":       name,
	}))
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// --- File, checkpoint, and history/tree helpers -------------------------------
//
// These wrap the drun-mcp file and checkpoint-timeline primitives the member
// console (ADR-0004) needs for its file view, checkpoint timeline, and
// diff-before-commit flow. They follow the same conventions as the helpers
// above: a typed string return where there is one obvious value, otherwise the
// raw *CallToolResult.

// SessionReadFile returns the contents of the file at path in the session,
// decoded to text. path is session-relative (e.g. "src/main.go"). For large
// files the underlying tool supports offset/limit paging; this helper reads the
// whole file — use CallTool directly if you need to page.
func (c *Client) SessionReadFile(ctx context.Context, sessionID, path string) (string, error) {
	res, err := ensureOK(c.CallTool(ctx, "session_read_file", map[string]any{
		"session_id": sessionID,
		"path":       path,
	}))
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// sessionExportPayload is the shape session_export returns: the host paths it
// wrote and the output directory it wrote them under.
type sessionExportPayload struct {
	ExportedFiles []string `json:"exported_files"`
	OutputDir     string   `json:"output_dir"`
}

// SessionExport writes the session's sandbox-generated files — those created
// inside the sandbox with no host origin — to outputDir on the host, and returns
// the paths it wrote. It is the companion to SessionCommit: commit writes back
// only *modified mounted* files, so newly created files are invisible to it and
// need export. outputDir must be under the engine's configured export_root
// (drun rejects other targets with export_denied); pass "" to use the engine's
// default export directory. With outputDir set, files land directly in it (e.g.
// outputDir/hello.py), so the whole host write is performed by drun.
func (c *Client) SessionExport(ctx context.Context, sessionID, outputDir string) ([]string, error) {
	args := map[string]any{"session_id": sessionID}
	if outputDir != "" {
		args["output_dir"] = outputDir
	}
	res, err := ensureOK(c.CallTool(ctx, "session_export", args))
	if err != nil {
		return nil, err
	}
	var out sessionExportPayload
	if err := res.Decode(&out); err != nil {
		return nil, err
	}
	return out.ExportedFiles, nil
}

// SessionWriteFile creates or overwrites the file at path with content,
// producing a new checkpoint. path is session-relative. Returns the raw tool
// result (checkpoint metadata in the payload).
func (c *Client) SessionWriteFile(ctx context.Context, sessionID, path, content string) (*CallToolResult, error) {
	return ensureOK(c.CallTool(ctx, "session_write_file", map[string]any{
		"session_id": sessionID,
		"path":       path,
		"content":    content,
	}))
}

// SessionCheckpoint marks the session's current checkpoint with a
// human-readable label and returns the tool's payload as text.
//
// drun creates checkpoints automatically on every write/bash call, so there is
// no explicit "create checkpoint" tool. The checkpoint primitive the console's
// timeline needs is labeling milestones ("baseline", "before commit"), so this
// wraps session_checkpoint_label to mark the current checkpoint. A non-empty
// label sets it; an empty label clears it, matching drun semantics.
func (c *Client) SessionCheckpoint(ctx context.Context, sessionID, label string) (string, error) {
	res, err := ensureOK(c.CallTool(ctx, "session_checkpoint_label", map[string]any{
		"session_id": sessionID,
		"label":      label,
	}))
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// SessionHistory returns the raw result of session_history: every checkpoint in
// the session with its stdout/stderr byte counts and the file delta relative to
// the previous checkpoint. Use CallToolResult.Decode to unmarshal the timeline.
func (c *Client) SessionHistory(ctx context.Context, sessionID string) (*CallToolResult, error) {
	return ensureOK(c.CallTool(ctx, "session_history", map[string]any{
		"session_id": sessionID,
	}))
}

// SessionTree returns the raw result of session_tree: the full checkpoint tree
// across all sessions, with forks nested under the checkpoint they branched
// from and the active head of each session flagged.
//
// session_tree is a global view and takes no arguments; sessionID is accepted
// for symmetry with the other session helpers but is intentionally not sent.
func (c *Client) SessionTree(ctx context.Context, sessionID string) (*CallToolResult, error) {
	return ensureOK(c.CallTool(ctx, "session_tree", map[string]any{}))
}

// SessionSnapshot serializes the session's full checkpoint history to a .drun
// file on the host and returns the raw result. The written path is in the
// payload (use CallToolResult.Text or Decode to read it); reload it with the
// engine's session_restore tool.
func (c *Client) SessionSnapshot(ctx context.Context, sessionID string) (*CallToolResult, error) {
	return ensureOK(c.CallTool(ctx, "session_snapshot", map[string]any{
		"session_id": sessionID,
	}))
}
