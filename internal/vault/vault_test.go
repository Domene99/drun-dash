package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// secretEnv is the environment variable EnvVault maps "vault://dev/anthropic"
// onto. Tests register it via t.Setenv so they never touch a real secret.
const secretEnv = "DRUNDASH_SECRET_ANTHROPIC"

func TestEnvVaultResolve(t *testing.T) {
	const wantKey = "sk-ant-test-value"

	tests := []struct {
		name      string
		ref       string
		setEnv    map[string]string // env vars to register for this case
		wantValue string
		wantErr   error // sentinel expected via errors.Is; nil means success
	}{
		{
			name:      "valid ref resolves to env value",
			ref:       "vault://dev/anthropic",
			setEnv:    map[string]string{secretEnv: wantKey},
			wantValue: wantKey,
		},
		{
			name:      "hyphenated name folds to underscore",
			ref:       "vault://dev/openai-us",
			setEnv:    map[string]string{"DRUNDASH_SECRET_OPENAI_US": "sk-openai"},
			wantValue: "sk-openai",
		},
		{
			name:      "empty env value still resolves",
			ref:       "vault://dev/anthropic",
			setEnv:    map[string]string{secretEnv: ""},
			wantValue: "",
		},
		{
			name:    "known scheme but unset env is not found",
			ref:     "vault://dev/anthropic",
			wantErr: ErrNotFound,
		},
		{
			name:    "missing scheme is invalid",
			ref:     "dev/anthropic",
			wantErr: ErrInvalidRef,
		},
		{
			name:    "wrong scheme is invalid",
			ref:     "https://dev/anthropic",
			wantErr: ErrInvalidRef,
		},
		{
			name:    "non-dev backend is invalid",
			ref:     "vault://prod/anthropic",
			wantErr: ErrInvalidRef,
		},
		{
			name:    "no backend separator is invalid",
			ref:     "vault://anthropic",
			wantErr: ErrInvalidRef,
		},
		{
			name:    "empty name is invalid",
			ref:     "vault://dev/",
			wantErr: ErrInvalidRef,
		},
		{
			name:    "nested name is invalid",
			ref:     "vault://dev/team/anthropic",
			wantErr: ErrInvalidRef,
		},
		{
			name:    "illegal name character is invalid",
			ref:     "vault://dev/anthropic.key",
			wantErr: ErrInvalidRef,
		},
		{
			name:    "empty ref is invalid",
			ref:     "",
			wantErr: ErrInvalidRef,
		},
	}

	v := NewEnvVault()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, val := range tt.setEnv {
				t.Setenv(k, val)
			}

			got, err := v.Resolve(context.Background(), tt.ref)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve(%q) error = %v, want %v", tt.ref, err, tt.wantErr)
				}
				if got.Value != "" {
					t.Errorf("Resolve(%q) returned non-empty Secret on error", tt.ref)
				}
				return
			}

			if err != nil {
				t.Fatalf("Resolve(%q) unexpected error: %v", tt.ref, err)
			}
			if got.Value != tt.wantValue {
				t.Errorf("Resolve(%q).Value = %q, want %q", tt.ref, got.Value, tt.wantValue)
			}
		})
	}
}

// TestResolveErrorNeverLeaksValue guards the promise that an error path never
// carries the credential, even though this backend cannot leak it (the value
// is read only on success). It documents the invariant for future backends.
func TestResolveErrorNeverLeaksValue(t *testing.T) {
	const secret = "sk-must-not-leak"
	t.Setenv(secretEnv, secret)

	// A malformed ref never reads the env var, so its error cannot contain the
	// value regardless.
	_, err := NewEnvVault().Resolve(context.Background(), "vault://prod/anthropic")
	if err == nil {
		t.Fatal("expected error for non-dev backend")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error text leaked secret value: %v", err)
	}
}

// TestSecretRedaction verifies a Secret never renders its raw value through
// the fmt verbs that logging commonly uses.
func TestSecretRedaction(t *testing.T) {
	const raw = "sk-ant-super-secret"
	s := Secret{Value: raw}

	if got := s.String(); strings.Contains(got, raw) {
		t.Errorf("String() = %q, leaks raw value", got)
	}
	if got := s.String(); got != redacted {
		t.Errorf("String() = %q, want %q", got, redacted)
	}

	// %s, %v, %+v, %#v, and a Secret embedded in a struct must all redact.
	cases := []string{
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%v", struct{ S Secret }{s}),
		fmt.Sprintf("%s", &s), // pointer also satisfies Stringer via value method
	}
	for _, got := range cases {
		if strings.Contains(got, raw) {
			t.Errorf("formatted %q leaks raw value", got)
		}
	}

	// The value is still retrievable when explicitly requested.
	if s.Value != raw {
		t.Errorf("Value = %q, want %q", s.Value, raw)
	}
}

// TestEnvVaultImplementsVault is a compile-and-run check that NewEnvVault
// yields something usable through the interface.
func TestEnvVaultImplementsVault(t *testing.T) {
	var v Vault = NewEnvVault()
	t.Setenv(secretEnv, "sk-ok")
	got, err := v.Resolve(context.Background(), "vault://dev/anthropic")
	if err != nil {
		t.Fatalf("Resolve via interface: %v", err)
	}
	if got.Value != "sk-ok" {
		t.Errorf("Value = %q, want %q", got.Value, "sk-ok")
	}
}
