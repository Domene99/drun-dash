package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSigningKeyFromSeed verifies a valid base64 seed yields the deterministic
// Ed25519 key derived from that seed.
func TestSigningKeyFromSeed(t *testing.T) {
	var seed [ed25519.SeedSize]byte
	for i := range seed {
		seed[i] = byte(i)
	}
	t.Setenv(envJWTSigningSeed, base64.StdEncoding.EncodeToString(seed[:]))

	key, err := signingKeyFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("signingKeyFromEnv: %v", err)
	}
	want := ed25519.NewKeyFromSeed(seed[:])
	if !key.Equal(want) {
		t.Error("derived key does not match the key from the seed")
	}
}

// TestSigningKeyEphemeral verifies an unset seed produces a usable key rather
// than an error (the local-dev convenience path).
func TestSigningKeyEphemeral(t *testing.T) {
	t.Setenv(envJWTSigningSeed, "")

	key, err := signingKeyFromEnv(discardLogger())
	if err != nil {
		t.Fatalf("signingKeyFromEnv: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Errorf("key size = %d, want %d", len(key), ed25519.PrivateKeySize)
	}
}

func TestSigningKeyInvalidSeed(t *testing.T) {
	tests := map[string]string{
		"not base64":   "!!!not-base64!!!",
		"wrong length": base64.StdEncoding.EncodeToString([]byte("too-short")),
	}
	for name, seed := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envJWTSigningSeed, seed)
			if _, err := signingKeyFromEnv(discardLogger()); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestGetEnvDefault(t *testing.T) {
	t.Setenv(envJWTIssuer, "")
	if got := getEnv(envJWTIssuer, defaultJWTIssuer); got != defaultJWTIssuer {
		t.Errorf("getEnv unset = %q, want %q", got, defaultJWTIssuer)
	}
	t.Setenv(envJWTIssuer, "custom")
	if got := getEnv(envJWTIssuer, defaultJWTIssuer); got != "custom" {
		t.Errorf("getEnv set = %q, want custom", got)
	}
}
