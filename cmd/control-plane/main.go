// Command control-plane runs the drun-dash REST API and admin UI server.
//
// This is a v1 scaffold: it exposes only liveness/readiness endpoints and
// graceful shutdown. Auth, database access, and the admin UI (templ+HTMX)
// arrive in later batches.
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
	"github.com/Domene99/drun-dash/internal/httpserver"
	"github.com/Domene99/drun-dash/internal/httpx"
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

// run loads configuration, builds the router, and serves until an interrupt or
// termination signal triggers a graceful shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("control-plane starting", slog.String("version", version), slog.String("env", cfg.Env))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return httpserver.New(cfg, newRouter(logger), logger).Run(ctx)
}

// newRouter builds the HTTP router for the control-plane server, wrapping every
// route in the shared middleware chain: request IDs, panic recovery, and
// structured access logging.
func newRouter(logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Recoverer(logger), httpx.Logger(logger))

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz)
	return r
}

// healthz reports server liveness.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether the server is ready to serve traffic. It is static for
// now and always reports ready.
//
// TODO(batch N): check downstream dependencies (database, Redis) and report 503
// when any are unavailable.
func readyz(w http.ResponseWriter, _ *http.Request) {
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
