package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Domene99/drun-dash/internal/cli"
)

// main is a one-liner that delegates to cli.Run and calls os.Exit, so the
// behavior is unit-tested in internal/cli. This smoke test confirms the wiring:
// the same dispatch main() uses is reachable and returns the version.
func TestRunWiring(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != cli.Version {
		t.Errorf("version output = %q, want %q", got, cli.Version)
	}
}
