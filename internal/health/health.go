// Package health provides the readiness abstraction behind the control-plane's
// GET /readyz endpoint.
//
// Liveness ("is the process running?") and readiness ("can the process serve
// traffic — are its dependencies reachable?") are distinct signals. Liveness is
// a static 200 owned by each binary; readiness is dependency-backed and lives
// here so orchestrators and load balancers can drain a pod whose database is
// down without killing it.
//
// A [Checker] verifies a single dependency. [Handler] runs a set of named
// checkers under a short per-check timeout and reports 200 {"status":"ready"}
// when all pass, or 503 with a flat [httpx.ErrorBody] naming the first failed
// dependency. [PingChecker] is the concrete checker for PostgreSQL, backed by
// [db.Pool.Ping].
package health

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/httpx"
)

// CodeNotReady is the machine-readable error code returned by [Handler] when a
// readiness check fails. It is distinct from the httpx.Code* set because "not
// ready" is an availability signal, not a request error.
const CodeNotReady = "not_ready"

// checkTimeout bounds each individual readiness check. A hung dependency (for
// example a database that accepts the connection but never answers) must not be
// able to block the readyz endpoint, so every check runs under this deadline.
const checkTimeout = 2 * time.Second

// Checker verifies that a single dependency is reachable and healthy. Check
// returns nil when the dependency is ready and a non-nil error describing the
// failure otherwise. Implementations must honor ctx cancellation.
type Checker interface {
	Check(ctx context.Context) error
}

// CheckerFunc adapts an ordinary function to the [Checker] interface.
type CheckerFunc func(ctx context.Context) error

// Check calls f(ctx).
func (f CheckerFunc) Check(ctx context.Context) error { return f(ctx) }

// PingChecker reports PostgreSQL readiness by pinging the shared connection
// pool. Because [db.Connect] is lazy, Ping is what actually verifies that the
// database is reachable.
type PingChecker struct {
	Pool *db.Pool
}

// Check pings the pool, returning any connectivity error.
func (c PingChecker) Check(ctx context.Context) error {
	return c.Pool.Ping(ctx)
}

// Named pairs a [Checker] with the dependency name reported to clients when the
// check fails.
type Named struct {
	Name    string
	Checker Checker
}

// Handler returns an [http.HandlerFunc] that runs every checker in order, each
// under its own [checkTimeout] deadline derived from the request context.
//
// It writes 200 {"status":"ready"} when every checker passes. On the first
// failure it writes a 503 with a flat [httpx.ErrorBody] whose code is
// [CodeNotReady] and whose message names the failed dependency; remaining
// checkers are not run. With no checkers it reports ready.
func Handler(checkers ...Named) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, c := range checkers {
			ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
			err := c.Checker.Check(ctx)
			cancel()
			if err != nil {
				_ = httpx.WriteError(w, http.StatusServiceUnavailable, CodeNotReady,
					fmt.Sprintf("dependency %q not ready: %v", c.Name, err))
				return
			}
		}
		_ = httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
