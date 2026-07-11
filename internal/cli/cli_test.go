package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout []string // substrings expected on stdout
		wantStderr []string // substrings expected on stderr
	}{
		{
			name:       "no args prints usage on stderr",
			args:       nil,
			wantCode:   2,
			wantStderr: []string{"Usage:", "login", "whoami", "config", "version"},
		},
		{
			name:       "help prints usage on stdout",
			args:       []string{"help"},
			wantCode:   0,
			wantStdout: []string{"Usage:", "login", "whoami", "config"},
		},
		{
			name:       "-h alias prints usage on stdout",
			args:       []string{"-h"},
			wantCode:   0,
			wantStdout: []string{"Usage:"},
		},
		{
			name:       "version prints the build version",
			args:       []string{"version"},
			wantCode:   0,
			wantStdout: []string{Version},
		},
		{
			name:       "--version alias prints the build version",
			args:       []string{"--version"},
			wantCode:   0,
			wantStdout: []string{Version},
		},
		{
			name:       "unknown command errors with usage",
			args:       []string{"bogus"},
			wantCode:   2,
			wantStderr: []string{"unknown command", "Usage:"},
		},
		{
			name:       "login -h prints command usage",
			args:       []string{"login", "-h"},
			wantCode:   2,
			wantStderr: []string{"Usage: drun-dash login", "device-authorization"},
		},
		{
			name:       "whoami -h prints command usage",
			args:       []string{"whoami", "-h"},
			wantCode:   2,
			wantStderr: []string{"Usage: drun-dash whoami", "/auth/whoami"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := Run(tt.args, &out, &errOut)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			for _, want := range tt.wantStdout {
				if !strings.Contains(out.String(), want) {
					t.Errorf("stdout %q missing %q", out.String(), want)
				}
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("stderr %q missing %q", errOut.String(), want)
				}
			}
		})
	}
}

// TestConfigCmd checks the config subcommand prints resolved values. It runs
// with whatever environment the test process has; config.Load applies defaults
// for anything unset, so at minimum the field labels and an ENV value appear.
func TestConfigCmd(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"config"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	for _, want := range []string{"DATABASE_URL", "REDIS_URL", "HTTP_ADDR", "LOG_LEVEL", "ENV"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("config stdout %q missing field %q", out.String(), want)
		}
	}
}

// TestEnvAlias verifies `env` dispatches to the same handler as `config`.
func TestEnvAlias(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"env"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Resolved configuration:") {
		t.Errorf("env stdout missing config output: %q", out.String())
	}
}
