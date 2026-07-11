// Command llm-proxy runs the drun-dash OpenAI-compatible LLM call broker.
//
// This is a v1 scaffold: it exposes a health endpoint and a placeholder broker
// route (see internal/proxy), plus graceful shutdown. The credential-brokering
// and provider-routing logic arrives in later batches.
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

// run loads configuration, builds the router, and serves until an interrupt or
// termination signal triggers a graceful shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := log.New(cfg)
	logger.Info("llm-proxy starting", slog.String("version", version), slog.String("env", cfg.Env))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return httpserver.New(proxyServerConfig(cfg), proxy.NewRouter(logger), logger).Run(ctx)
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
