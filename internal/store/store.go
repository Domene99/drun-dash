// Package store is drun-dash's PostgreSQL persistence layer: a set of small,
// hand-written repositories that map the control-plane schema to Go types.
//
// This package is the reference implementation every later store package
// (catalog, budgets, telemetry, …) copies, so it deliberately favours a small,
// explicit, idiomatic surface over cleverness:
//
//   - One repository type per aggregate (e.g. [OrganizationStore]), constructed
//     with a New… function that takes a [Querier].
//   - [Querier] is the narrow subset of the pgx pool used by the repositories.
//     A *[github.com/Domene99/drun-dash/internal/db.Pool] satisfies it directly
//     (it embeds *pgxpool.Pool), and so does a [pgx.Tx], so the same repository
//     methods run inside or outside a transaction.
//   - Hand-written SQL with positional parameters ($1, $2, …). No ORM, no query
//     builder.
//   - Driver errors never leak: lookups translate [pgx.ErrNoRows] to the
//     package sentinel [ErrNotFound], and unique-constraint violations to
//     [ErrConflict]. Callers branch with errors.Is.
//
// Every method takes a context.Context as its first argument and wraps returned
// errors with operation context.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors returned by the stores. Callers should compare with
// errors.Is rather than matching on error strings.
var (
	// ErrNotFound is returned by the GetBy… lookups (and mutations that target
	// a specific row) when no matching row exists. It translates the driver's
	// pgx.ErrNoRows so callers never depend on pgx directly.
	ErrNotFound = errors.New("store: not found")

	// ErrConflict is returned when a write violates a unique constraint —
	// a duplicate organization slug, a duplicate (org_id, name), a member
	// already on a team, and so on. The wrapped error names the offending
	// constraint for diagnostics.
	ErrConflict = errors.New("store: conflict")
)

// pgUniqueViolation is the SQLSTATE code Postgres reports for a unique- or
// primary-key-constraint violation (unique_violation, class 23). Declared here
// to avoid taking a direct dependency on the pgerrcode package.
const pgUniqueViolation = "23505"

// Querier is the subset of the pgx pool API the repositories depend on. Both
// *pgxpool.Pool (via internal/db.Pool) and pgx.Tx implement it, so a repository
// can run against the shared pool or inside a caller-managed transaction
// without change.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Compile-time assertions that the concrete pgx types callers pass in satisfy
// Querier. If pgx changes a signature this fails to build here rather than at
// every call site.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = pgx.Tx(nil)
)

// mapError normalises a driver error into a package sentinel:
//
//   - pgx.ErrNoRows            → ErrNotFound
//   - unique_violation (23505) → ErrConflict (naming the constraint)
//   - anything else            → returned unchanged
//
// It returns nil for a nil error so call sites can wrap unconditionally.
// Callers are expected to add operation context around the result, e.g.
// fmt.Errorf("store: create organization: %w", mapError(err)).
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return fmt.Errorf("%w on %s", ErrConflict, pgErr.ConstraintName)
	}
	return err
}
