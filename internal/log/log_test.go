package log

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Domene99/drun-dash/internal/config"
)

func TestLevelFor(t *testing.T) {
	tests := []struct {
		name  string
		level string
		want  slog.Level
	}{
		{"debug", config.LogLevelDebug, slog.LevelDebug},
		{"info", config.LogLevelInfo, slog.LevelInfo},
		{"warn", config.LogLevelWarn, slog.LevelWarn},
		{"error", config.LogLevelError, slog.LevelError},
		{"unknown falls back to info", "bogus", slog.LevelInfo},
		{"empty falls back to info", "", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := levelFor(tt.level); got != tt.want {
				t.Errorf("levelFor(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

// TestNewLevelWired verifies the configured level actually reaches the handler
// by checking which levels the returned logger reports as enabled.
func TestNewLevelWired(t *testing.T) {
	cfg := &config.Config{LogLevel: config.LogLevelWarn, Env: config.EnvDevelopment}
	logger := New(cfg)

	ctx := context.Background()
	if logger.Enabled(ctx, slog.LevelInfo) {
		t.Error("info should be disabled at warn level")
	}
	if !logger.Enabled(ctx, slog.LevelWarn) {
		t.Error("warn should be enabled at warn level")
	}
	if !logger.Enabled(ctx, slog.LevelError) {
		t.Error("error should be enabled at warn level")
	}
}

func TestNewHandlerType(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string // concrete handler type we expect
	}{
		{"production uses JSON", config.EnvProduction, "*slog.JSONHandler"},
		{"development uses text", config.EnvDevelopment, "*slog.TextHandler"},
		{"staging uses text", config.EnvStaging, "*slog.TextHandler"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{LogLevel: config.LogLevelInfo, Env: tt.env}
			logger := New(cfg)

			switch tt.want {
			case "*slog.JSONHandler":
				if _, ok := logger.Handler().(*slog.JSONHandler); !ok {
					t.Errorf("env %q: got %T, want *slog.JSONHandler", tt.env, logger.Handler())
				}
			case "*slog.TextHandler":
				if _, ok := logger.Handler().(*slog.TextHandler); !ok {
					t.Errorf("env %q: got %T, want *slog.TextHandler", tt.env, logger.Handler())
				}
			}
		})
	}
}
