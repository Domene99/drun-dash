# drunmcp

A minimal, dependency-free Go client for **drun-mcp** — the MCP server exposed
by the drun sandbox. It speaks MCP over the **Streamable HTTP** transport and
provides typed helpers for the session lifecycle tools the drun-dash CLI needs.

Module path: `github.com/Domene99/drun-dash/pkg/drunmcp`
(stdlib only — `net/http` + `encoding/json`).

## Transport requirements

drun-mcp uses **Streamable HTTP**, not the SSE-only GET transport. Every call
is a single HTTP `POST` to the MCP endpoint. The server replies with **either**:

- `Content-Type: application/json` — one JSON-RPC response, or
- `Content-Type: text/event-stream` — a Server-Sent Events stream whose frames
  carry the JSON-RPC response (plus any server log frames).

This client handles both automatically.

| Fact | Value |
| --- | --- |
| Endpoint | `http://127.0.0.1:7273/mcp` (`DefaultEndpoint`) |
| Method | `POST` |
| Required `Accept` header | `application/json, text/event-stream` (`AcceptHeader`) |
| Health check | none — probe with `Initialize` (no `/health` endpoint) |
| Port | `7273`, hardcoded in drun today |

The `Accept` header is sent on **every** request (including notifications).
Dropping `text/event-stream` from it is the most common integration mistake —
see below.

## Failure modes

These map directly to the drun-mcp skill documentation:

- **`406 Not Acceptable`** — the request reached drun-mcp but the `Accept`
  header did not advertise `text/event-stream`. This client always sends the
  correct header; a 406 usually means a proxy stripped or rewrote it. The
  returned error names the required header value.
- **`404 Not Found`** — there is no MCP endpoint at the requested path. drun-mcp
  serves MCP at `/mcp` and has **no** `/health` endpoint, so hitting `/health`
  (or any other path) returns 404. Probe liveness by calling `Initialize`
  instead. The returned error explains this.

Other non-2xx statuses are returned with the response body snippet for
debugging. JSON-RPC-level errors come back as `*RPCError`.

## Usage

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

func main() {
	ctx := context.Background()
	c := drunmcp.NewClient(drunmcp.DefaultEndpoint)

	// No /health endpoint — Initialize doubles as the liveness probe.
	if _, err := c.Initialize(ctx); err != nil {
		log.Fatalf("drun-mcp not available: %v", err)
	}

	sid, err := c.CreateSession(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer c.SessionClose(context.Background(), sid)

	res, err := c.SessionBash(ctx, sid, "echo hello")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Text())
}
```

### Generic tool calls

For tools without a typed helper, use `CallTool` directly:

```go
res, err := c.CallTool(ctx, "session_write_file", map[string]any{
	"session_id": sid,
	"path":       "main.go",
	"content":    "package main",
})
```

`CallToolResult` offers `Text()` (concatenated text blocks) and `Decode(v)`
(unmarshal the structured payload — prefers `structuredContent`, falls back to
the JSON text block that drun-mcp session tools return).

## Typed session helpers

Wrappers with the exact argument names drun-mcp expects:

| Method | Tool |
| --- | --- |
| `CreateSession` | `create_session` |
| `SessionClose` | `session_close` |
| `SessionBash` | `session_bash` |
| `SessionMount` | `session_mount` |
| `SessionFork` | `session_fork` |
| `SessionRollback` | `session_rollback` |
| `SessionDiff` | `session_diff` |
| `SessionCommit` | `session_commit` |
| `GetFetchAllowlist` | `get_fetch_allowlist` |
| `SessionGetEnv` | `session_get_env` |
| `SessionReadFile` | `session_read_file` |
| `SessionWriteFile` | `session_write_file` |
| `SessionCheckpoint` | `session_checkpoint_label` |
| `SessionHistory` | `session_history` |
| `SessionTree` | `session_tree` |
| `SessionSnapshot` | `session_snapshot` |

Checkpoints are addressed with `CheckpointRef` (`AtCheckpoint(id)` or
`AtLabel(label)`); label takes precedence when both are set, matching drun-mcp.

Two of the file/checkpoint helpers wrap the engine's real wire shape rather
than a one-to-one tool name:

- **`SessionCheckpoint`** wraps `session_checkpoint_label`. drun creates
  checkpoints automatically on every write/bash call, so there is no explicit
  "create checkpoint" tool; the timeline primitive is labeling the current
  checkpoint (a non-empty label sets it, an empty label clears it).
- **`SessionTree`** wraps `session_tree`, which is a **global** checkpoint tree
  and takes no arguments. `sessionID` is accepted for symmetry with the other
  helpers but is not sent on the wire.

## Testing

Unit tests use `httptest` mock servers and need no running drun-mcp:

```sh
go test ./...
```

Integration tests are behind the `integration` build tag and are skipped in CI
by default. They require a drun-mcp server already listening on
`DefaultEndpoint` (override with `DRUNMCP_ENDPOINT`); this package never starts
the process itself. If the server is unreachable they skip rather than fail:

```sh
go test -tags integration ./...
```
