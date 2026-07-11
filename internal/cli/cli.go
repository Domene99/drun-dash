// Package cli implements the drun-dash developer CLI: command tree,
// flag parsing, help output, and dispatch. The cmd/drun-dash binary is a thin
// wrapper that calls [Run]; keeping the logic here lets tests exercise every
// subcommand in-process with buffers instead of spawning a subprocess.
//
// Following cmd/policyctl, dispatch is a stdlib switch on the first argument
// with a per-command flag.FlagSet — no cobra/urfave or other CLI framework.
//
// drun-dash is the power-user surface that authenticates a member to the org,
// syncs policy, and drives governed drun sessions. Most subcommands are still
// stubs: [Run] establishes the command structure now so later batches can fill
// in real auth (device-authorization flow), policy sync, and session capture
// without reworking dispatch. Stubs print what they will do and exit non-zero.
package cli

import (
	"fmt"
	"io"
)

// Version is the build version reported by `drun-dash version` and
// `drun-dash --version`. It defaults to "dev" and is overridable at build time
// via -ldflags "-X github.com/Domene99/drun-dash/internal/cli.Version=<value>".
var Version = "dev"

// Run dispatches a subcommand and returns a process exit code. It writes normal
// output to stdout and errors/usage to stderr (help requested explicitly goes
// to stdout). It never calls os.Exit, so tests can call it directly with
// bytes.Buffers.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch cmd := args[0]; cmd {
	case "version", "--version":
		fmt.Fprintln(stdout, Version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "login":
		return loginCmd(args[1:], stdout, stderr)
	case "whoami":
		return whoamiCmd(args[1:], stdout, stderr)
	case "config", "env":
		return configCmd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "drun-dash: unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

// usage writes the top-level help text listing every subcommand.
func usage(w io.Writer) {
	fmt.Fprint(w, `drun-dash — developer CLI for drun-dash

Usage:
  drun-dash <command> [flags]

Commands:
  login      Authenticate this machine to the org (stub — not implemented)
  whoami     Show the currently authenticated member (stub — not implemented)
  config     Print the resolved runtime configuration
  version    Print the CLI version
  help       Show this help text

Run "drun-dash <command> -h" for command-specific flags.
`)
}
