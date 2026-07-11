//go:build integration

// The RedisRevoker tests exercise a real Redis 7 instance. They are excluded
// from the default build and only compile/run under the `integration` build
// tag, mirroring internal/redis:
//
//	go test -tags integration ./internal/auth/...
//
// Requirements:
//   - A reachable Redis. By default they use config.DefaultRedisURL (the local
//     Docker dev stack in deploy/docker); set REDIS_URL to override.
//
// If Redis is unreachable the tests SKIP rather than fail, so a machine with no
// server does not produce a red build.
package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Domene99/drun-dash/internal/config"
	"github.com/Domene99/drun-dash/internal/redis"
)

func integrationRedisURL() string {
	if v := os.Getenv("REDIS_URL"); v != "" {
		return v
	}
	return config.DefaultRedisURL
}

// requireRevoker connects to Redis and returns a RedisRevoker with a
// test-scoped key prefix, skipping the test if Redis is unreachable.
func requireRevoker(t *testing.T) (*RedisRevoker, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	cfg := &config.Config{RedisURL: integrationRedisURL()}
	client, err := redis.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("redis.Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Ping(ctx); err != nil {
		t.Skipf("redis not reachable at %s (%v); start it and rerun with -tags integration", cfg.RedisURL, err)
	}

	revoker, err := NewRedisRevoker(client, WithKeyPrefix("drun-dash:test:authz:revoked:jti:"))
	if err != nil {
		t.Fatalf("NewRedisRevoker: %v", err)
	}
	return revoker, ctx
}

func TestIntegrationRevokeThenIsRevoked(t *testing.T) {
	revoker, ctx := requireRevoker(t)
	const jti = "integration-jti-revoked"
	t.Cleanup(func() { _ = revoker.client.Del(ctx, revoker.key(jti)).Err() })

	revoked, err := revoker.IsRevoked(ctx, jti)
	if err != nil {
		t.Fatalf("IsRevoked (pre): %v", err)
	}
	if revoked {
		t.Fatal("IsRevoked before Revoke = true, want false")
	}

	if err := revoker.Revoke(ctx, jti, time.Minute); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	revoked, err = revoker.IsRevoked(ctx, jti)
	if err != nil {
		t.Fatalf("IsRevoked (post): %v", err)
	}
	if !revoked {
		t.Fatal("IsRevoked after Revoke = false, want true")
	}
}

// A revocation entry must carry a TTL so the denylist self-expires; it must
// never persist indefinitely.
func TestIntegrationRevokeSetsTTL(t *testing.T) {
	revoker, ctx := requireRevoker(t)
	const jti = "integration-jti-ttl"
	t.Cleanup(func() { _ = revoker.client.Del(ctx, revoker.key(jti)).Err() })

	if err := revoker.Revoke(ctx, jti, 90*time.Second); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	ttl, err := revoker.client.TTL(ctx, revoker.key(jti)).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > 90*time.Second {
		t.Fatalf("TTL = %s, want (0, 90s]", ttl)
	}
}

// An entry with a short TTL disappears on its own, so a token becomes admissible
// again exactly when it would have expired anyway.
func TestIntegrationRevocationExpires(t *testing.T) {
	revoker, ctx := requireRevoker(t)
	const jti = "integration-jti-expiry"
	t.Cleanup(func() { _ = revoker.client.Del(ctx, revoker.key(jti)).Err() })

	if err := revoker.Revoke(ctx, jti, time.Second); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	revoked, err := revoker.IsRevoked(ctx, jti)
	if err != nil {
		t.Fatalf("IsRevoked: %v", err)
	}
	if revoked {
		t.Fatal("IsRevoked after TTL elapsed = true, want false (entry should self-expire)")
	}
}

func TestIntegrationRevokeRejectsBadInput(t *testing.T) {
	revoker, ctx := requireRevoker(t)
	if err := revoker.Revoke(ctx, "", time.Minute); err == nil {
		t.Error("Revoke with empty jti: want error")
	}
	if err := revoker.Revoke(ctx, "jti", 0); err == nil {
		t.Error("Revoke with zero ttl: want error")
	}
	if err := revoker.Revoke(ctx, "jti", -time.Second); err == nil {
		t.Error("Revoke with negative ttl: want error")
	}
	if _, err := revoker.IsRevoked(ctx, ""); err == nil {
		t.Error("IsRevoked with empty jti: want error")
	}
}
