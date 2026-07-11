// Command drun-dash is the developer CLI for drun-dash.
//
// This is a v1 scaffold: only the `version` and `help` subcommands are wired
// up. Authentication and session subcommands (login, session) arrive in later
// batches.
//
// The CLI uses the standard library's flag/subcommand dispatch rather than a
// third-party framework (e.g. cobra) to avoid adding a dependency for the
// current scaffold scope.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is overridable at build time via
// -ldflags "-X main.version=<value>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code. It is
// separated from main so tests can exercise it without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch cmd := args[0]; cmd {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "drun-dash: unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

// usage writes the top-level help text.
func usage(w io.Writer) {
	fmt.Fprint(w, `drun-dash - developer CLI for drun-dash

Usage:
  drun-dash <command>

Commands:
  version    Print the CLI version
  help       Show this help text

More commands (login, session) arrive in a later release.
`)
}
