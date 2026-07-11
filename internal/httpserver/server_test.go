package httpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/config"
)

// freeAddr reserves an ephemeral localhost port and returns it as a host:port
// string. The listener is closed before returning, so the address is free for
// the server under test to bind (a small race window, acceptable for tests).
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	return addr
}

func TestNewSetsServerFields(t *testing.T) {
	cfg := &config.Config{HTTPAddr: "127.0.0.1:12345"}
	srv := New(cfg, http.NotFoundHandler(), nil)

	if srv.httpServer.Addr != cfg.HTTPAddr {
		t.Errorf("Addr = %q, want %q", srv.httpServer.Addr, cfg.HTTPAddr)
	}
	if srv.httpServer.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.httpServer.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.httpServer.Handler == nil {
		t.Error("Handler = nil, want the provided handler")
	}
	if srv.logger == nil {
		t.Error("logger = nil, want slog.Default fallback")
	}
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	addr := freeAddr(t)
	cfg := &config.Config{HTTPAddr: addr}

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	srv := New(cfg, h, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Poll until the server accepts connections, then verify it serves.
	url := "http://" + addr + "/"
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		resp, err = http.Get(url) //nolint:bodyclose // closed below
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became reachable: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	// Cancelling the context must trigger a clean graceful shutdown.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(shutdownGrace + time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRunReturnsListenError(t *testing.T) {
	// A negative port is a syntactically valid host:port that the kernel
	// rejects at bind time, forcing ListenAndServe to fail.
	cfg := &config.Config{HTTPAddr: "127.0.0.1:-1"}
	srv := New(cfg, http.NotFoundHandler(), nil)

	err := srv.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil, want a listen error")
	}
}
