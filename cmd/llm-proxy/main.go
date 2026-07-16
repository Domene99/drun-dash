// Command llm-proxy runs the drun-dash OpenAI-compatible LLM call broker.
//
// It exposes a health endpoint and forwards POST /v1/chat/completions to a
// single configured upstream (see internal/proxy), gating every call on a
// per-session credential grant and metering it (ADR-0003 §3/§5), plus graceful
// shutdown. The grant validation and metering resolve through the shared
// database pool this command opens; provider-routing across multiple endpoints
// arrives in a later batch.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/httpserver"
	"github.com/Domene99/drun-dash/internal/log"
	"github.com/Domene99/drun-dash/internal/proxy"
	"github.com/Domene99/drun-dash/internal/store"
	"github.com/Domene99/drun-dash/internal/vault"
)

// version is overridable at build time via
// -ldflags "-X main.version=<value>".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		slog.Error("llm-proxy exited", slog.Any("error", err))
		os.Exit(1)
	}
}

// run loads configuration, opens the database pool, builds the router, and
// serves until an interrupt or termination signal triggers a graceful
// shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(cfg)
	logger.Info("llm-proxy starting", slog.String("version", version), slog.String("env", cfg.Env))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Connect is lazy: it validates the connection string but does not dial, so
	// this succeeds even when Postgres is momentarily down. The broker's
	// per-call grant lookup and metering dial the pool on first use; a real
	// outage surfaces as a 500/failed-metering per request, not a boot failure.
	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Build the composite vault the broker resolves upstream credentials
	// through (ADR-0009): db-backed secrets (vault://db/*) sealed by the control
	// plane under DRUNDASH_VAULT_KEY, plus the env-backed dev backend
	// (vault://dev/*). An unset/invalid key leaves the DB backend disabled — the
	// proxy still boots and dev-backed secrets keep resolving; a launch pinned
	// to a db-backed endpoint then fails closed at call time with a clear
	// credential-unavailable error rather than crashing the server.
	dbVault, err := vault.NewDBVault(store.NewSecretStore(pool), vaultKeyFromEnv(logger))
	if err != nil {
		return fmt.Errorf("llm-proxy: build db vault: %w", err)
	}
	composite := vault.NewCompositeVault(dbVault, vault.NewEnvVault())

	return httpserver.New(proxyServerConfig(cfg), proxy.NewRouter(logger, pool, composite), logger).Run(ctx)
}

// envVaultKey names the environment variable holding the vault master key: the
// base64-encoded 32-byte AES-256 key that seals and opens db-backed secrets
// (ADR-0009). It must be the same value on the control plane and the proxy, so a
// key sealed by one opens on the other.
const envVaultKey = "DRUNDASH_VAULT_KEY"

// vaultKeyFromEnv decodes the master key from DRUNDASH_VAULT_KEY. An unset
// variable returns nil, which NewDBVault treats as a disabled backend (the
// service boots; db-backed secrets are unavailable until the key is set). A
// set-but-malformed value (not base64, or not 32 bytes) is logged and also
// treated as absent, so a bad key never crashes the boot.
func vaultKeyFromEnv(logger *slog.Logger) []byte {
	raw := os.Getenv(envVaultKey)
	if raw == "" {
		logger.Warn("DRUNDASH_VAULT_KEY not set; db-backed secrets (vault://db/*) are unavailable",
			slog.String("hint", "set DRUNDASH_VAULT_KEY to a base64-encoded 32-byte key to enable the writable vault"))
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		logger.Error("DRUNDASH_VAULT_KEY is not valid base64; db-backed secrets disabled", slog.Any("error", err))
		return nil
	}
	if len(key) != 32 {
		logger.Error("DRUNDASH_VAULT_KEY must decode to 32 bytes; db-backed secrets disabled",
			slog.Int("got", len(key)))
		return nil
	}
	return key
}

// proxyServerConfig returns a shallow copy of cfg whose HTTPAddr is the proxy's
// listen address (ProxyHTTPAddr). httpserver.New binds cfg.HTTPAddr (the
// control-plane address), so the copy lets the proxy reuse the shared server
// package verbatim without modifying it.
func proxyServerConfig(cfg *config.Config) *config.Config {
	proxyCfg := *cfg
	proxyCfg.HTTPAddr = cfg.ProxyHTTPAddr
	return &proxyCfg
}
