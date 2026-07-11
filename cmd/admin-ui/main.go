// Command admin-ui runs the drun-dash server-rendered admin web app.
//
// It is a separate binary from the control plane: it renders a login page and a
// dashboard (Go + templ + HTMX) and reads live data from the control-plane REST
// API over HTTP, never importing control-plane packages. The operator signs in
// by pasting a control-plane session token, which is stored in a secure,
// HttpOnly cookie and forwarded as a Bearer credential.
//
// Configuration mirrors the other binaries: shared settings come from
// [config.Load]; the two admin-ui-specific settings come straight from the
// environment (internal/config is shared and not extended per-binary):
//
//   - ADMIN_UI_HTTP_ADDR    listen address (default :8082)
//   - CONTROL_PLANE_BASE_URL control-plane base URL (default http://localhost:8080)
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
	"github.com/Domene99/drun-dash/internal/ui"
)

// version is overridable at build time via
// -ldflags "-X main.version=<value>".
var version = "dev"

// Admin-ui environment variables and their defaults. internal/config is shared
// by every binary and not extended per-binary, so the admin UI reads its two
// settings straight from the environment (documented in cmd/admin-ui and
// internal/ui/README.md).
const (
	envAdminUIHTTPAddr  = "ADMIN_UI_HTTP_ADDR"
	envControlPlaneBase = "CONTROL_PLANE_BASE_URL"

	defaultAdminUIHTTPAddr  = ":8082"
	defaultControlPlaneBase = "http://localhost:8080"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		slog.Error("admin-ui exited", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads configuration, builds the control-plane client and UI handler, and
// serves until an interrupt or termination signal triggers a graceful
// shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(cfg)

	adminAddr := getEnv(envAdminUIHTTPAddr, defaultAdminUIHTTPAddr)
	if err := validateAddr(adminAddr); err != nil {
		return err
	}
	baseURL := getEnv(envControlPlaneBase, defaultControlPlaneBase)

	logger.Info("admin-ui starting",
		slog.String("version", version),
		slog.String("env", cfg.Env),
		slog.String("addr", adminAddr),
		slog.String("control_plane_base_url", baseURL),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := ui.NewClient(baseURL, nil)
	// Secure cookies require HTTPS; enable them only in production so plain-HTTP
	// local development still works.
	handler := ui.New(client, logger, ui.WithSecureCookie(cfg.IsProduction()))

	return httpserver.New(adminServerConfig(cfg, adminAddr), handler, logger).Run(ctx)
}

// adminServerConfig returns a shallow copy of cfg whose HTTPAddr is the
// admin-ui listen address. httpserver.New binds cfg.HTTPAddr (the control-plane
// address), so the copy lets the admin UI reuse the shared server package
// verbatim without modifying it — the same trick cmd/llm-proxy uses.
func adminServerConfig(cfg *config.Config, addr string) *config.Config {
	adminCfg := *cfg
	adminCfg.HTTPAddr = addr
	return &adminCfg
}

// getEnv returns the value of key, or def when the variable is unset or empty.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// validateAddr checks that addr is a valid host:port listen address, failing
// fast at boot rather than on first bind. An empty host (":8082") is allowed
// and binds all interfaces.
func validateAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("admin-ui: %s=%q must be host:port: %w", envAdminUIHTTPAddr, addr, err)
	}
	return nil
}
