// Package log centralizes construction of the structured [slog.Logger] used by
// every drun-dash binary (control-plane, llm-proxy, and the CLI).
//
// All binaries build their logger the same way via [New]: JSON output in
// production, human-readable text output everywhere else, with the verbosity
// driven by config. The resulting logger is what the internal/httpx middleware
// ([httpx.Recoverer], [httpx.Logger]) expects.
//
// [New] never calls [slog.SetDefault]; installing the logger as the process
// default is left to each binary's main so tests and libraries stay free of
// global state.
package log

import (
	"log/slog"
	"os"

	"github.com/Domene99/drun-dash/internal/config"
)

// New builds a [slog.Logger] from cfg.
//
// The handler is selected by environment: [slog.NewJSONHandler] when
// cfg.IsProduction reports true, otherwise the more readable
// [slog.NewTextHandler]. The minimum level is derived from cfg.LogLevel via
// [levelFor]. Output is written to [os.Stdout].
//
// New does not call [slog.SetDefault]; the caller decides whether to install
// the returned logger as the process default.
func New(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: levelFor(cfg.LogLevel)}

	var handler slog.Handler
	if cfg.IsProduction() {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// levelFor maps a config.LogLevel* string to its [slog.Level]. Any
// unrecognized value falls back to [slog.LevelInfo].
func levelFor(level string) slog.Level {
	switch level {
	case config.LogLevelDebug:
		return slog.LevelDebug
	case config.LogLevelInfo:
		return slog.LevelInfo
	case config.LogLevelWarn:
		return slog.LevelWarn
	case config.LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
