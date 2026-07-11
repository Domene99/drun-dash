package config

import (
	"errors"
	"os"
	"testing"
)

// configVars is every environment variable this package reads.
var configVars = []string{
	"DATABASE_URL", "REDIS_URL", "HTTP_ADDR",
	"PROXY_HTTP_ADDR", "LOG_LEVEL", "ENV",
}

// unsetEnv removes key for the duration of the test, restoring its prior value
// (or unset state) via t.Cleanup. t.Setenv cannot clear a variable, so we
// manage the restore ourselves.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestLoadDefaults(t *testing.T) {
	// Clear any config vars leaking in from the ambient environment so we
	// exercise the built-in defaults.
	for _, k := range configVars {
		unsetEnv(t, k)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() with no env set: %v", err)
	}

	// DATABASE_URL default must match deploy/docker/.env.example so the
	// package works against the local dev stack out of the box.
	const wantDB = "postgres://drundash:drundash@localhost:5432/drundash?sslmode=disable"
	if DefaultDatabaseURL != wantDB {
		t.Errorf("DefaultDatabaseURL drifted from documented dev-stack value: %q", DefaultDatabaseURL)
	}
	if c.DatabaseURL != wantDB {
		t.Errorf("DatabaseURL = %q, want %q", c.DatabaseURL, wantDB)
	}
	if c.RedisURL != DefaultRedisURL {
		t.Errorf("RedisURL = %q, want %q", c.RedisURL, DefaultRedisURL)
	}
	if c.HTTPAddr != DefaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", c.HTTPAddr, DefaultHTTPAddr)
	}
	if c.ProxyHTTPAddr != DefaultProxyHTTPAddr {
		t.Errorf("ProxyHTTPAddr = %q, want %q", c.ProxyHTTPAddr, DefaultProxyHTTPAddr)
	}
	if c.LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel = %q, want %q", c.LogLevel, DefaultLogLevel)
	}
	if c.Env != DefaultEnv {
		t.Errorf("Env = %q, want %q", c.Env, DefaultEnv)
	}
	if c.IsProduction() {
		t.Errorf("IsProduction() = true for default env %q", c.Env)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/app?sslmode=require")
	t.Setenv("REDIS_URL", "rediss://cache:6380/1")
	t.Setenv("HTTP_ADDR", "0.0.0.0:9090")
	t.Setenv("PROXY_HTTP_ADDR", "127.0.0.1:9091")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("ENV", "production")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if c.DatabaseURL != "postgres://u:p@db:5432/app?sslmode=require" {
		t.Errorf("DatabaseURL = %q", c.DatabaseURL)
	}
	if c.RedisURL != "rediss://cache:6380/1" {
		t.Errorf("RedisURL = %q", c.RedisURL)
	}
	if c.HTTPAddr != "0.0.0.0:9090" {
		t.Errorf("HTTPAddr = %q", c.HTTPAddr)
	}
	if c.ProxyHTTPAddr != "127.0.0.1:9091" {
		t.Errorf("ProxyHTTPAddr = %q", c.ProxyHTTPAddr)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
	if !c.IsProduction() {
		t.Errorf("IsProduction() = false for ENV=production")
	}
}

// TestLoadEmptyStringIsExplicit verifies that a variable set to "" is treated
// as an explicit empty value (a validation failure) rather than falling back
// to the default.
func TestLoadEmptyStringIsExplicit(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); !errors.Is(err, ErrMissingField) {
		t.Fatalf("Load() with DATABASE_URL=\"\" = %v, want ErrMissingField", err)
	}
}

func TestValidate(t *testing.T) {
	base := func() *Config {
		return &Config{
			DatabaseURL:   DefaultDatabaseURL,
			RedisURL:      DefaultRedisURL,
			HTTPAddr:      DefaultHTTPAddr,
			ProxyHTTPAddr: DefaultProxyHTTPAddr,
			LogLevel:      DefaultLogLevel,
			Env:           DefaultEnv,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr error // nil means valid
	}{
		{"valid defaults", func(*Config) {}, nil},
		{"empty database url", func(c *Config) { c.DatabaseURL = "" }, ErrMissingField},
		{"empty redis url", func(c *Config) { c.RedisURL = "" }, ErrMissingField},
		{"empty http addr", func(c *Config) { c.HTTPAddr = "" }, ErrMissingField},
		{"empty proxy addr", func(c *Config) { c.ProxyHTTPAddr = "" }, ErrMissingField},
		{"empty log level", func(c *Config) { c.LogLevel = "" }, ErrMissingField},
		{"empty env", func(c *Config) { c.Env = "" }, ErrMissingField},
		{"db wrong scheme", func(c *Config) { c.DatabaseURL = "mysql://h:3306/db" }, ErrInvalidValue},
		{"db no host", func(c *Config) { c.DatabaseURL = "postgres:///db" }, ErrInvalidValue},
		{"redis wrong scheme", func(c *Config) { c.RedisURL = "http://h:6379" }, ErrInvalidValue},
		{"http addr not host:port", func(c *Config) { c.HTTPAddr = "8080" }, ErrInvalidValue},
		{"proxy addr not host:port", func(c *Config) { c.ProxyHTTPAddr = "nope" }, ErrInvalidValue},
		{"unknown log level", func(c *Config) { c.LogLevel = "trace" }, ErrInvalidValue},
		{"unknown env", func(c *Config) { c.Env = "qa" }, ErrInvalidValue},
		{"postgresql scheme ok", func(c *Config) { c.DatabaseURL = "postgresql://u:p@h:5432/db" }, nil},
		{"rediss scheme ok", func(c *Config) { c.RedisURL = "rediss://h:6379/0" }, nil},
		{"bare port addr ok", func(c *Config) { c.HTTPAddr = ":80" }, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			err := c.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadInvalidFailsFast(t *testing.T) {
	t.Setenv("LOG_LEVEL", "loud")
	if _, err := Load(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("Load() with bad LOG_LEVEL = %v, want ErrInvalidValue", err)
	}
}
