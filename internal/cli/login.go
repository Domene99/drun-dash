package cli

import (
	"flag"
	"fmt"
	"io"
)

// loginCmd is a stub for the `login` subcommand. It parses flags and describes
// the planned OAuth 2.0 device-authorization flow, then exits non-zero to make
// clear that authentication is not implemented yet.
//
// TODO(batch/5): implement the real device-authorization flow — request a
// device/user code from the control plane, prompt the operator to visit the
// verification URL, poll the token endpoint, and persist the resulting token
// (keychain/secure storage). No token storage or network calls happen yet.
func loginCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: drun-dash login

Authenticate this machine to the org via the OAuth 2.0
device-authorization flow.

Not implemented yet (planned for a later batch).
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Fprint(stderr, `drun-dash login is not implemented yet.

When implemented, it will:
  1. Request a device and user code from the control plane.
  2. Print a verification URL and user code for you to open in a browser.
  3. Poll the token endpoint until you approve the request.
  4. Store the resulting access token securely for later commands.
`)
	return 1
}
