package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/Domene99/drun-dash/internal/config"
)

// configCmd loads the runtime configuration via config.Load and prints the
// resolved values. It is also reachable as `env`. The config package holds no
// secrets, so every field is printed verbatim.
//
// This is a real (non-stub) command: it exercises config.Load end to end,
// giving operators a quick way to see what the CLI resolved from the
// environment and its defaults.
func configCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash config

Load and print the resolved runtime configuration (from environment
variables and package defaults). No values are secret.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "drun-dash config: %v\n", err)
		return 1
	}

	fmt.Fprint(stdout, "Resolved configuration:\n")
	fmt.Fprintf(stdout, "  DATABASE_URL     %s\n", cfg.DatabaseURL)
	fmt.Fprintf(stdout, "  REDIS_URL        %s\n", cfg.RedisURL)
	fmt.Fprintf(stdout, "  HTTP_ADDR        %s\n", cfg.HTTPAddr)
	fmt.Fprintf(stdout, "  PROXY_HTTP_ADDR  %s\n", cfg.ProxyHTTPAddr)
	fmt.Fprintf(stdout, "  LOG_LEVEL        %s\n", cfg.LogLevel)
	fmt.Fprintf(stdout, "  ENV              %s\n", cfg.Env)
	return 0
}
