// Command admin-ui runs the drun-dash server-rendered admin web app.
//
// It is a separate binary from the control plane: it renders a login page and a
// dashboard (Go + templ + HTMX) and reads live data from the control-plane REST
// API over HTTP, never importing control-plane packages. Members sign in
// through the OIDC authorization-code browser flow; the minted control-plane
// session JWT is stored in a secure, HttpOnly cookie and forwarded as a Bearer
// credential. When OIDC is not configured the UI falls back to a paste-a-token
// dev login so local development still works.
//
// Configuration mirrors the other binaries: shared settings come from
// [config.Load]; the admin-ui-specific settings come straight from the
// environment (internal/config is shared and not extended per-binary):
//
//   - ADMIN_UI_HTTP_ADDR       listen address (default :8082)
//   - CONTROL_PLANE_BASE_URL   control-plane base URL (default http://localhost:8080)
//   - OIDC_ISSUER              identity-provider issuer URL (enables OIDC login)
//   - OIDC_CLIENT_ID           registered OAuth client ID
//   - OIDC_CLIENT_SECRET       OAuth client secret (confidential clients)
//   - ADMIN_UI_REDIRECT_URL    this app's /callback URL registered with the IdP
//   - DEFAULT_ORGANIZATION_ID  org forwarded to the control plane's /auth/session
//
// The OIDC group is optional: when OIDC_ISSUER, OIDC_CLIENT_ID, and
// ADMIN_UI_REDIRECT_URL are all set the browser flow is enabled, otherwise the
// paste-a-token dev login stays active.
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
// by every binary and not extended per-binary, so the admin UI reads its own
// settings straight from the environment (documented in cmd/admin-ui and
// internal/ui/README.md). The OIDC settings are read by ui.OIDCConfigFromEnv.
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

// run loads configuration, builds the control-plane client and UI handler
// (wiring the OIDC authenticator when configured), and serves until an
// interrupt or termination signal triggers a graceful shutdown.
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
	// local development still works. The environment name surfaces in the
	// top-bar indicator (uikit TopBar).
	opts := []ui.Option{
		ui.WithSecureCookie(cfg.IsProduction()),
		ui.WithEnvironment(cfg.Env),
	}

	oidcCfg := ui.OIDCConfigFromEnv()
	if oidcCfg.Enabled() {
		authr, err := ui.NewOIDCAuthenticator(ctx, oidcCfg)
		if err != nil {
			return fmt.Errorf("admin-ui: configure oidc: %w", err)
		}
		opts = append(opts, ui.WithOIDC(authr))
		logger.Info("admin-ui: OIDC authorization-code login enabled",
			slog.String("oidc_issuer", oidcCfg.Issuer),
			slog.String("redirect_url", oidcCfg.RedirectURL),
		)
	} else {
		logger.Warn("admin-ui: OIDC not configured; using paste-a-token dev login")
	}

	handler := ui.New(client, logger, opts...)

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
