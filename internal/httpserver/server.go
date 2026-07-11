// Package httpserver provides a small, reusable graceful HTTP server used by
// every drun-dash HTTP binary (control-plane, llm-proxy).
//
// Each binary shares the same server lifecycle: build a router, apply shared
// middleware, listen, and shut down cleanly when the process receives a
// termination signal. That lifecycle lives here so the binaries only differ in
// the handler they serve.
//
// The server is deliberately transport-agnostic: [New] takes an
// [http.Handler], so callers own routing and middleware (see internal/httpx).
// [Server.Run] blocks until its context is cancelled, then drains in-flight
// requests within a bounded grace period.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Domene99/drun-dash/internal/config"
)

// Tuning constants for the underlying [http.Server]. They are intentionally
// conservative and shared by all binaries; callers that need different values
// can build their own server.
const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers, mitigating slow-header (Slowloris-style) attacks.
	readHeaderTimeout = 10 * time.Second
	// shutdownGrace bounds how long Run waits for in-flight requests to drain
	// during graceful shutdown before giving up.
	shutdownGrace = 15 * time.Second
)

// Server wraps an [http.Server] with graceful-shutdown lifecycle management.
// Construct it with [New] and drive it with [Server.Run]; the zero value is
// not usable.
type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

// New builds a Server that serves h on the address from cfg.HTTPAddr. The
// logger is used for lifecycle messages (listening, shutting down); pass nil to
// use [slog.Default].
//
// New does not bind the listen socket — call [Server.Run] to start serving.
func New(cfg *config.Config, h http.Handler, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           h,
			ReadHeaderTimeout: readHeaderTimeout,
		},
		logger: logger,
	}
}

// Run starts the server and blocks until ctx is cancelled or the server fails
// to serve. On cancellation it stops accepting new connections and drains
// in-flight requests within shutdownGrace, then returns.
//
// It returns nil on a clean shutdown, the listen error if the server could not
// serve (other than the expected [http.ErrServerClosed]), or the shutdown
// error if draining did not complete within the grace period.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http server listening", slog.String("addr", s.httpServer.Addr))
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// The server stopped on its own (typically a listen failure) before
		// the context was cancelled.
		return err
	case <-ctx.Done():
		s.logger.Info("http server shutting down", slog.String("addr", s.httpServer.Addr))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// Wait for ListenAndServe to return so its goroutine cannot outlive Run.
	return <-errCh
}
