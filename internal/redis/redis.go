// Package redis is the single shared Redis access layer for drun-dash.
//
// Every binary that talks to Redis (control-plane today, more later) obtains
// its client from [Connect], which builds a
// [github.com/redis/go-redis/v9.Client] from [config.Config.RedisURL]. Redis 7
// backs the request-path machinery of drun-dash — budget counters, short-lived
// credential/entitlement caches, and telemetry buffering — but this package
// owns connectivity only; it deliberately defines no counters, caches, rate
// limiters, or telemetry buffers. Those belong to later batches that embed or
// wrap the [Client] exposed here.
//
// Construction is lazy: [Connect] parses the connection string and prepares the
// client but does not dial Redis. Call [Client.Ping] to verify that the server
// is actually reachable (a readyz check does exactly this), and [Client.Close]
// to release the connection pool at shutdown.
//
// This mirrors the shape and lifecycle of [github.com/Domene99/drun-dash/internal/db]
// so both connectivity layers feel identical to callers.
package redis

import (
	"context"
	"fmt"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/redis/go-redis/v9"
)

// Client is drun-dash's handle to Redis. It embeds *redis.Client, so the full
// go-redis command surface (Get, Set, Incr, Eval, …) is available to downstream
// packages, while [Client.Ping] and [Client.Close] provide the lifecycle
// operations most callers need.
type Client struct {
	*redis.Client
}

// Connect builds a Redis client from cfg.RedisURL. It parses and validates the
// connection string but does not open a network connection — go-redis dials
// lazily on first command — so Connect succeeds even when Redis is momentarily
// unreachable. Use [Client.Ping] to confirm connectivity.
//
// The returned client must be released with [Client.Close].
func Connect(ctx context.Context, cfg *config.Config) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("redis: connect: nil config")
	}

	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("redis: parse redis url: %w", err)
	}

	return &Client{Client: redis.NewClient(opt)}, nil
}
