package cli

import (
	"flag"
	"fmt"
	"io"
)

// whoamiCmd is a stub for the `whoami` subcommand. Because login does not yet
// store a token, there is never an authenticated member to report, so it
// prints a "not logged in" message and exits non-zero.
//
// TODO(batch/5): once login persists a token, read it, validate/introspect it
// against the control plane, and print the authenticated member and org.
func whoamiCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash whoami

Show the currently authenticated member.

Not implemented yet (planned for a later batch).
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Fprint(stderr, `Not logged in.

drun-dash whoami is not implemented yet. Once `+"`drun-dash login`"+` stores a
token, this command will report the authenticated member and org.
`)
	return 1
}
