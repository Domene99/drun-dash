// Command member-console runs the drun-dash member conversational console.
//
// It is a separate binary from the control plane and a localhost web app an
// organization member runs on their own workstation: it renders a device-flow
// sign-in page, lists the agents the member is entitled to launch, launches or
// terminates a governed session, and drives a conversational run view where a
// natural-language directive becomes governed agent actions — model calls
// through the proxy, tool calls in the local drun sandbox, streamed live with
// structure-only trajectory sent to the control plane (ADR-0004). It reads live
// data from the control-plane REST API over HTTP, never importing control-plane
// packages, and talks locally to the drun engine via pkg/drunmcp. The member
// signs in through the OAuth 2.0 device-authorization flow (RFC 8628): the
// console requests a device+user code, the member approves it in the admin UI,
// and the console polls for the minted control-plane session JWT, which it
// stores in a secure, HttpOnly cookie and forwards as a Bearer credential.
//
// Configuration mirrors the other binaries: shared settings come from
// [config.Load]; the console-specific settings come straight from the
// environment (internal/config is shared and not extended per-binary):
//
//   - MEMBER_CONSOLE_HTTP_ADDR    listen address (default :8083)
//   - CONTROL_PLANE_BASE_URL      control-plane base URL (default http://localhost:8080)
//   - MEMBER_CONSOLE_CLIENT_ID    device-flow client_id (default drun-dash-member-console)
//   - MEMBER_CONSOLE_MODEL        model id passed to the governed proxy (required for runs)
//   - DRUN_MCP_ENDPOINT           local drun engine endpoint (default drunmcp.DefaultEndpoint)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/httpserver"
	"github.com/Domene99/drun-dash/internal/log"
	"github.com/Domene99/drun-dash/internal/memberui"
	"github.com/Domene99/drun-dash/pkg/drunmcp"
)

// version is overridable at build time via
// -ldflags "-X main.version=<value>".
var version = "dev"

// Member-console environment variables and their defaults. internal/config is
// shared by every binary and not extended per-binary, so the console reads its
// own settings straight from the environment (documented above and in
// internal/memberui/README.md).
const (
	envMemberConsoleHTTPAddr = "MEMBER_CONSOLE_HTTP_ADDR"
	envControlPlaneBase      = "CONTROL_PLANE_BASE_URL"
	envMemberConsoleClientID = "MEMBER_CONSOLE_CLIENT_ID"
	envMemberConsoleModel    = "MEMBER_CONSOLE_MODEL"
	envDrunMCPEndpoint       = "DRUN_MCP_ENDPOINT"

	defaultMemberConsoleHTTPAddr = ":8083"
	defaultControlPlaneBase      = "http://localhost:8080"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		slog.Error("member-console exited", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads configuration, builds the control-plane client and console handler,
// and serves until an interrupt or termination signal triggers a graceful
// shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(cfg)

	addr := getEnv(envMemberConsoleHTTPAddr, defaultMemberConsoleHTTPAddr)
	if err := validateAddr(addr); err != nil {
		return err
	}
	baseURL := getEnv(envControlPlaneBase, defaultControlPlaneBase)
	model := os.Getenv(envMemberConsoleModel)
	drunEndpoint := getEnv(envDrunMCPEndpoint, drunmcp.DefaultEndpoint)

	logger.Info("member-console starting",
		slog.String("version", version),
		slog.String("env", cfg.Env),
		slog.String("addr", addr),
		slog.String("control_plane_base_url", baseURL),
		slog.String("drun_mcp_endpoint", drunEndpoint),
		slog.Bool("model_configured", model != ""),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := memberui.NewClient(baseURL, nil)

	opts := []memberui.Option{
		// Secure cookies require HTTPS; enable them only in production so
		// plain-HTTP localhost development still works.
		memberui.WithSecureCookie(cfg.IsProduction()),
		memberui.WithModel(model),
		memberui.WithDrunEndpoint(drunEndpoint),
	}
	if clientID := os.Getenv(envMemberConsoleClientID); clientID != "" {
		opts = append(opts, memberui.WithClientID(clientID))
	}

	handler := memberui.New(client, logger, opts...)

	return httpserver.New(memberServerConfig(cfg, addr), handler, logger).Run(ctx)
}

// memberServerConfig returns a shallow copy of cfg whose HTTPAddr is the
// console's listen address. httpserver.New binds cfg.HTTPAddr (the
// control-plane address), so the copy lets the console reuse the shared server
// package verbatim without modifying it — the same trick cmd/llm-proxy and
// cmd/admin-ui use.
func memberServerConfig(cfg *config.Config, addr string) *config.Config {
	memberCfg := *cfg
	memberCfg.HTTPAddr = addr
	return &memberCfg
}

// getEnv returns the value of key, or def when the variable is unset or empty.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// validateAddr checks that addr is a valid host:port listen address, failing
// fast at boot rather than on first bind. An empty host (":8083") is allowed
// and binds all interfaces.
func validateAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("member-console: %s=%q must be host:port: %w", envMemberConsoleHTTPAddr, addr, err)
	}
	return nil
}
