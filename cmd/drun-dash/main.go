// Command drun-dash is the developer CLI for drun-dash — the power-user surface
// that authenticates a member to the org, syncs policy, and drives governed
// drun sessions.
//
// This binary is intentionally thin: all command-tree, flag-parsing, and
// dispatch logic lives in github.com/Domene99/drun-dash/internal/cli so it can
// be unit-tested in-process. Most subcommands are still stubs (see that
// package); real auth, policy sync, and session control arrive in later
// batches.
//
// The CLI uses stdlib flag/subcommand dispatch rather than a third-party
// framework (e.g. cobra) to avoid adding a dependency.
package main

import (
	"os"

	"github.com/Domene99/drun-dash/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
