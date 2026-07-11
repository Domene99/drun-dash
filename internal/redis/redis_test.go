package redis

import (
	"context"
	"testing"

	"github.com/Domene99/drun-dash/internal/config"
)

// These tests exercise the URL-handling and error paths that need no live
// Redis. Anything requiring a real server lives in integration_test.go, behind
// the `integration` build tag.

func TestConnectNilConfig(t *testing.T) {
	if _, err := Connect(context.Background(), nil); err == nil {
		t.Fatal("Connect(nil) = nil error, want error")
	}
}

func TestConnectInvalidRedisURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"wrong scheme", "http://localhost:6379/0"},
		{"unparseable", "://not a url"},
		{"bad database number", "redis://localhost:6379/notanumber"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{RedisURL: tt.url}
			if _, err := Connect(context.Background(), cfg); err == nil {
				t.Fatalf("Connect with %s URL %q = nil error, want error", tt.name, tt.url)
			}
		})
	}
}

// Connect must succeed and dial lazily: a well-formed URL yields a usable client
// even with no Redis reachable, and that client exposes the embedded go-redis
// command surface plus the Close lifecycle helper.
func TestConnectLazyValidURL(t *testing.T) {
	cfg := &config.Config{RedisURL: config.DefaultRedisURL}
	client, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Connect(%q) error: %v", cfg.RedisURL, err)
	}
	if client == nil || client.Client == nil {
		t.Fatal("Connect returned a nil client")
	}
	// The embedded *redis.Client is promoted, so options parsed from the URL
	// are visible without a live server (no dial happened in Connect).
	if got := client.Options().Addr; got != "localhost:6379" {
		t.Fatalf("client Addr = %q, want %q", got, "localhost:6379")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
