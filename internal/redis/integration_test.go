//go:build integration

// The redis integration tests exercise a real Redis 7 instance. They are
// excluded from the default build and only compile/run under the `integration`
// build tag:
//
//	go test -tags integration ./internal/redis/...
//
// Requirements:
//   - A reachable Redis. By default they use config.DefaultRedisURL (the local
//     Docker dev stack in deploy/docker); set REDIS_URL to override.
//
// If Redis is unreachable the tests SKIP rather than fail, so a machine with no
// server does not produce a red build — mirroring internal/db.
package redis

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/config"
)

func integrationRedisURL() string {
	if v := os.Getenv("REDIS_URL"); v != "" {
		return v
	}
	return config.DefaultRedisURL
}

// requireClient connects and pings, skipping the test if Redis is unreachable.
func requireClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	cfg := &config.Config{RedisURL: integrationRedisURL()}
	client, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(ctx); err != nil {
		t.Skipf("redis not reachable at %s (%v); start it and rerun with -tags integration", cfg.RedisURL, err)
	}
	return client, ctx
}

func TestIntegrationConnectPingClose(t *testing.T) {
	client, ctx := requireClient(t)
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// The embedded *redis.Client must expose the full command surface later batches
// rely on. Exercise a round trip through the promoted methods.
func TestIntegrationSetGetIncr(t *testing.T) {
	client, ctx := requireClient(t)

	const key = "drun-dash:internal-redis:test:counter"
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })

	if err := client.Del(ctx, key).Err(); err != nil {
		t.Fatalf("Del: %v", err)
	}
	n, err := client.Incr(ctx, key).Result()
	if err != nil {
		t.Fatalf("Incr: %v", err)
	}
	if n != 1 {
		t.Fatalf("Incr = %d, want 1", n)
	}
	got, err := client.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "1" {
		t.Fatalf("Get = %q, want %q", got, "1")
	}
}
