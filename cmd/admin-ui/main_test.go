package main

import (
	"testing"

	"github.com/Domene99/drun-dash/internal/config"
)

// TestAdminServerConfig verifies the admin UI serves on its own address while
// leaving the loaded config untouched, so the shared httpserver package (which
// binds HTTPAddr) can be reused verbatim.
func TestAdminServerConfig(t *testing.T) {
	cfg := &config.Config{
		HTTPAddr:      ":8080",
		ProxyHTTPAddr: ":8081",
		LogLevel:      config.DefaultLogLevel,
		Env:           config.DefaultEnv,
	}

	got := adminServerConfig(cfg, ":8082")

	if got.HTTPAddr != ":8082" {
		t.Errorf("HTTPAddr = %q, want %q (the admin-ui address)", got.HTTPAddr, ":8082")
	}
	if got == cfg {
		t.Error("adminServerConfig returned the original config, want a copy")
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("original HTTPAddr mutated to %q, want %q", cfg.HTTPAddr, ":8080")
	}
	// Unrelated fields carry over unchanged.
	if got.ProxyHTTPAddr != ":8081" || got.LogLevel != cfg.LogLevel || got.Env != cfg.Env {
		t.Errorf("copy diverged from original in non-address fields: %+v", got)
	}
}

func TestGetEnv(t *testing.T) {
	const key = "ADMIN_UI_TEST_VAR"

	t.Run("unset falls back to default", func(t *testing.T) {
		if got := getEnv(key, "fallback"); got != "fallback" {
			t.Errorf("getEnv unset = %q, want %q", got, "fallback")
		}
	})
	t.Run("empty falls back to default", func(t *testing.T) {
		t.Setenv(key, "")
		if got := getEnv(key, "fallback"); got != "fallback" {
			t.Errorf("getEnv empty = %q, want %q", got, "fallback")
		}
	})
	t.Run("set value wins", func(t *testing.T) {
		t.Setenv(key, "override")
		if got := getEnv(key, "fallback"); got != "override" {
			t.Errorf("getEnv set = %q, want %q", got, "override")
		}
	})
}

func TestValidateAddr(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"bare port", ":8082", false},
		{"host and port", "127.0.0.1:8082", false},
		{"missing port", "8082", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAddr(tt.addr)
			if tt.wantErr && err == nil {
				t.Errorf("validateAddr(%q) = nil, want error", tt.addr)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateAddr(%q) = %v, want nil", tt.addr, err)
			}
		})
	}
}
