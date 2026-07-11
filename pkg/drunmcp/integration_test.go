//go:build integration

// Package drunmcp integration tests exercise a real drun-mcp server over the
// Streamable HTTP transport. They are excluded from the default build and only
// compile/run under the `integration` build tag:
//
//	go test -tags integration ./...
//
// Requirements:
//   - A drun-mcp server reachable at DefaultEndpoint (http://127.0.0.1:7273/mcp).
//     Start it separately; this package never launches the process itself.
//   - Set DRUNMCP_ENDPOINT to override the address if needed.
//
// If the server is unreachable, the tests SKIP rather than fail, so a
// misconfigured environment does not produce a red build. CI leaves the
// `integration` tag off, so these never run there by default.
package drunmcp

import (
	"context"
	"os"
	"testing"
	"time"
)

func integrationEndpoint() string {
	if v := os.Getenv("DRUNMCP_ENDPOINT"); v != "" {
		return v
	}
	return DefaultEndpoint
}

func newIntegrationClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	c := NewClient(integrationEndpoint())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if _, err := c.Initialize(ctx); err != nil {
		t.Skipf("drun-mcp not reachable at %s (%v); start it and rerun with -tags integration", integrationEndpoint(), err)
	}
	return c, ctx
}

func TestIntegrationInitializeAndListTools(t *testing.T) {
	c, ctx := newIntegrationClient(t)

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("expected at least one tool from a live drun-mcp server")
	}
	t.Logf("drun-mcp exposes %d tools", len(tools.Tools))
}

func TestIntegrationSessionLifecycle(t *testing.T) {
	c, ctx := newIntegrationClient(t)

	sid, err := c.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sid == "" {
		t.Fatal("CreateSession returned an empty session id")
	}
	t.Cleanup(func() {
		if err := c.SessionClose(context.Background(), sid); err != nil {
			t.Logf("SessionClose: %v", err)
		}
	})

	res, err := c.SessionBash(ctx, sid, "echo hello-drun")
	if err != nil {
		t.Fatalf("SessionBash: %v", err)
	}
	t.Logf("session_bash result: %s", res.Text())
}
