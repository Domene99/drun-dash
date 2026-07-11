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

	return httpserver.New(proxyServerConfig(cfg), proxy.NewRouter(logger, pool), logger).Run(ctx)
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
