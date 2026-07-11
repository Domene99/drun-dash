// Command control-plane runs the drun-dash REST API and admin UI server.
//
// This is a v1 scaffold: it exposes liveness/readiness endpoints and graceful
// shutdown. Readiness is backed by a database ping; auth and the admin UI
// (templ+HTMX) arrive in later batches.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/health"
	"github.com/Domene99/drun-dash/internal/httpserver"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/log"
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
		slog.Error("control-plane exited", slog.Any("error", err))
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
	logger.Info("control-plane starting", slog.String("version", version), slog.String("env", cfg.Env))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Connect is lazy: it validates the connection string but does not dial, so
	// this succeeds even when Postgres is momentarily down. The /readyz check
	// pings the pool to report actual connectivity.
	pool, err := db.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	router := newRouter(logger, health.Named{Name: "database", Checker: health.PingChecker{Pool: pool}})
	return httpserver.New(cfg, router, logger).Run(ctx)
}

// newRouter builds the HTTP router for the control-plane server, wrapping every
// route in the shared middleware chain: request IDs, panic recovery, and
// structured access logging. The readiness checkers back GET /readyz.
func newRouter(logger *slog.Logger, checkers ...health.Named) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer(logger), httpx.Logger(logger))

	r.Get("/healthz", healthz)
	r.Get("/readyz", health.Handler(checkers...))
	return r
}

// healthz reports server liveness. It is a static 200 and deliberately does not
// consult downstream dependencies, so a dependency blip cannot make an
// orchestrator kill an otherwise-healthy process.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
