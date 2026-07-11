// Package db is the single shared PostgreSQL access layer for drun-dash.
//
// Every binary that talks to the database (control-plane today, more later)
// obtains its connection pool from [Connect], which builds a
// [github.com/jackc/pgx/v5/pgxpool.Pool] from [config.Config.DatabaseURL].
// This package owns connectivity and schema migration only; it deliberately
// defines no per-table repositories, queries, or domain types — those belong
// to later batches that embed or wrap the [Pool] exposed here.
//
// Construction is lazy: [Connect] validates the connection string and prepares
// the pool but does not dial Postgres. Call [Pool.Ping] to verify that the
// database is actually reachable (the readyz check does exactly this), and
// [Pool.Close] to release every connection at shutdown.
package db

import (
	"context"
	"fmt"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is drun-dash's handle to PostgreSQL. It embeds *pgxpool.Pool, so the
// full pgx query surface (Query, QueryRow, Exec, Begin, …) is available to
// downstream repository packages, while [Pool.Ping] and [Pool.Close] provide
// the lifecycle operations most callers need.
type Pool struct {
	*pgxpool.Pool
}

// Connect builds a connection pool from cfg.DatabaseURL. It parses and
// validates the connection string but does not open a network connection —
// pgxpool dials lazily on first use — so Connect succeeds even when Postgres
// is momentarily unreachable. Use [Pool.Ping] to confirm connectivity.
//
// The returned pool must be released with [Pool.Close].
func Connect(ctx context.Context, cfg *config.Config) (*Pool, error) {
	if cfg == nil {
		return nil, fmt.Errorf("db: connect: nil config")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse database url: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: new pool: %w", err)
	}

	return &Pool{Pool: pool}, nil
}
