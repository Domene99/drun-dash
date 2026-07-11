package main

import (
	"testing"

	"github.com/Domene99/drun-dash/internal/config"
)

// TestProxyServerConfig verifies the proxy serves on ProxyHTTPAddr while
// leaving the loaded config untouched, so the shared httpserver package (which
// binds HTTPAddr) can be reused verbatim.
func TestProxyServerConfig(t *testing.T) {
	cfg := &config.Config{
		HTTPAddr:      ":8080",
		ProxyHTTPAddr: ":8081",
		LogLevel:      config.DefaultLogLevel,
		Env:           config.DefaultEnv,
	}

	got := proxyServerConfig(cfg)

	if got.HTTPAddr != ":8081" {
		t.Errorf("HTTPAddr = %q, want %q (the proxy address)", got.HTTPAddr, ":8081")
	}
	if got == cfg {
		t.Error("proxyServerConfig returned the original config, want a copy")
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("original HTTPAddr mutated to %q, want %q", cfg.HTTPAddr, ":8080")
	}
	// Unrelated fields carry over unchanged.
	if got.ProxyHTTPAddr != ":8081" || got.LogLevel != cfg.LogLevel || got.Env != cfg.Env {
		t.Errorf("copy diverged from original in non-address fields: %+v", got)
	}
}
