package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"version"}, &out, &errOut)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != version {
		t.Errorf("version output = %q, want %q", got, version)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}

func TestHelpSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"help"}, &out, &errOut)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("help output missing usage text: %q", out.String())
	}
}

func TestNoArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "Usage:") {
		t.Errorf("expected usage on stderr, got %q", errOut.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"bogus"}, &out, &errOut)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("expected unknown command error, got %q", errOut.String())
	}
}
