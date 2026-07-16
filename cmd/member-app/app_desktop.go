//go:build desktop

// Command member-app (desktop build) opens a native OS window over the
// in-process internal/memberui server. See main.go for the package overview and
// README.md for the build/packaging recipe.
//
// This file carries the "desktop" build tag (the counterpart to main.go's
// "!desktop"), so exactly one main() compiles per tag set. It imports the cgo
// webview library, so it is compiled only by `go build -tags desktop`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	webview "github.com/webview/webview_go"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/log"
	"github.com/Domene99/drun-dash/internal/memberui"
	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// version is overridable at build time via
// -ldflags "-X main.version=<value>".
var version = "dev"

// Environment variables and defaults, mirroring cmd/member-console
// (internal/config is shared by every binary and not extended per-binary, so
// the app reads its own settings straight from the environment).
// MEMBER_CONSOLE_HTTP_ADDR is intentionally absent: the desktop app always binds
// an ephemeral loopback port (ADR-0005 §1), so there is no listen address to
// configure.
const (
	envControlPlaneBase      = "CONTROL_PLANE_BASE_URL"
	envMemberConsoleClientID = "MEMBER_CONSOLE_CLIENT_ID"
	envMemberConsoleModel    = "MEMBER_CONSOLE_MODEL"
	envDrunMCPEndpoint       = "DRUN_MCP_ENDPOINT"
	envApprovalTimeout       = "MEMBER_CONSOLE_APPROVAL_TIMEOUT"
	envApprovalPollInterval  = "MEMBER_CONSOLE_APPROVAL_POLL_INTERVAL"

	defaultControlPlaneBase = "http://localhost:8080"
)

// Window and lifecycle tuning for the desktop shell. ADR-0005 §7 leaves the
// exact ergonomics (size/title) to the implementing batch, bounded only by §1
// (in-process loopback server, window owns the process lifecycle).
const (
	windowTitle  = "drun — member console"
	windowWidth  = 1280
	windowHeight = 860

	// readHeaderTimeout mirrors internal/httpserver: bound how long a client
	// may take to send request headers (Slowloris mitigation). The only client
	// is the app's own window, but the value is cheap insurance.
	readHeaderTimeout = 10 * time.Second
	// serverStartTimeout bounds how long we wait for the in-process server to
	// answer before opening the window.
	serverStartTimeout = 5 * time.Second
	// shutdownGrace bounds how long we wait for in-flight requests to drain
	// when the window closes (matches internal/httpserver).
	shutdownGrace = 15 * time.Second
)

// init locks the main goroutine to the main OS thread. The webview must be
// created and run on the process's main thread (Cocoa/AppKit on macOS, and the
// GTK/Win32 main loops elsewhere require it); locking here — before any other
// goroutine can preempt main onto a different OS thread — guarantees webview.Run
// blocks on the right thread.
func init() {
	runtime.LockOSThread()
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		slog.Error("member-app exited", slog.Any("error", err))
		os.Exit(1)
	}
}

// run builds the same control-plane client and internal/memberui handler that
// cmd/member-console wires, serves it on an ephemeral loopback port in-process,
// and opens a native window pointed at it. When the window closes it shuts the
// server down gracefully and returns — no orphaned server or listener.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(cfg)

	baseURL := getEnv(envControlPlaneBase, defaultControlPlaneBase)
	model := os.Getenv(envMemberConsoleModel)
	drunEndpoint := getEnv(envDrunMCPEndpoint, drunmcp.DefaultEndpoint)

	// Bind an ephemeral loopback port up front: reachable only from this
	// machine, fresh per launch, and never colliding with a separately-run
	// cmd/member-console (ADR-0005 §1). Binding before Serve also lets us read
	// back the OS-assigned port for the window URL.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("member-app: bind loopback: %w", err)
	}
	url := fmt.Sprintf("http://%s/", listener.Addr().String())

	logger.Info("member-app starting",
		slog.String("version", version),
		slog.String("env", cfg.Env),
		slog.String("url", url),
		slog.String("control_plane_base_url", baseURL),
		slog.String("drun_mcp_endpoint", drunEndpoint),
		slog.Bool("model_configured", model != ""),
	)

	client := memberui.NewClient(baseURL, nil)

	opts := []memberui.Option{
		// Secure cookies require HTTPS; enable them only in production so the
		// plain-HTTP loopback server the window talks to still works.
		memberui.WithSecureCookie(cfg.IsProduction()),
		memberui.WithModel(model),
		memberui.WithDrunEndpoint(drunEndpoint),
	}
	if clientID := os.Getenv(envMemberConsoleClientID); clientID != "" {
		opts = append(opts, memberui.WithClientID(clientID))
	}
	if d, err := parseDurationEnv(envApprovalTimeout); err != nil {
		return err
	} else if d > 0 {
		opts = append(opts, memberui.WithApprovalWaitTimeout(d))
	}
	if d, err := parseDurationEnv(envApprovalPollInterval); err != nil {
		return err
	} else if d > 0 {
		opts = append(opts, memberui.WithApprovalPollInterval(d))
	}

	handler := memberui.New(client, logger, opts...)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Serve on the already-bound listener. A clean Shutdown/Close returns the
	// expected http.ErrServerClosed, which is not an error to report.
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// The listener already queues the window's first connection, but wait until
	// the server actually answers before opening the window so the member never
	// sees a connection-refused flash. If the server dies during startup,
	// surface that instead of opening a dead window.
	if err := waitForServer(url, serverStartTimeout, serveErr); err != nil {
		_ = srv.Close()
		return err
	}

	// Open the native window on the main OS thread. Run blocks for the window's
	// life; it returns when the member closes the window.
	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle(windowTitle)
	w.SetSize(windowWidth, windowHeight, webview.HintNone)
	w.Navigate(url)
	w.Run()

	// Window closed → shut the in-process server down gracefully so no listener
	// or server goroutine outlives the window (ADR-0005 §1).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("member-app: server shutdown: %w", err)
	}
	logger.Info("member-app stopped")
	return <-serveErr
}

// waitForServer polls url until the in-process server answers, the timeout
// elapses, or the server goroutine reports a fatal serve error. Any HTTP
// response (including a redirect to /login or a 401) means the server is live.
func waitForServer(url string, timeout time.Duration, serveErr <-chan error) error {
	deadline := time.Now().Add(timeout)
	httpClient := &http.Client{Timeout: time.Second}
	for {
		select {
		case err := <-serveErr:
			// The server stopped before it ever answered.
			if err == nil {
				err = errors.New("server closed during startup")
			}
			return fmt.Errorf("member-app: in-process server failed to start: %w", err)
		default:
		}

		resp, err := httpClient.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("member-app: in-process server not ready after %s: %w", timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// parseDurationEnv reads an optional Go duration from the environment. An unset
// or empty variable yields 0 (use the default); a malformed value is a fatal
// misconfiguration surfaced at boot rather than silently ignored. Mirrors
// cmd/member-console.
func parseDurationEnv(key string) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("member-app: %s=%q must be a Go duration: %w", key, v, err)
	}
	return d, nil
}

// getEnv returns the value of key, or def when the variable is unset or empty.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
