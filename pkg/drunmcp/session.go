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
