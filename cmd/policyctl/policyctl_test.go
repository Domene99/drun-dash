package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unsignedBundle is a minimal, structurally valid bundle whose content_hash and
// signature are empty placeholders, as an operator would hand to `sign`. Its
// drun_config uses only fields pkg/policy models so a re-load accepts it.
const unsignedBundle = `{
  "schema_version": "1.0",
  "org_id": "org_test",
  "policy_id": "pol_test",
  "policy_version": 7,
  "drun_config": {
    "domain_allowlist": ["registry.npmjs.org", "pypi.org"],
    "mount_overlay_paths": [],
    "max_sessions": 2,
    "bash_timeout_ms": 30000
  },
  "issued_at": "",
  "expires_at": "",
  "content_hash": "",
  "signature": ""
}`

// freshWindow returns an [issued_at, expires_at] pair that brackets now so a
// freshly signed bundle passes the freshness check during the round trip.
func freshWindow() (string, string) {
	now := time.Now().UTC()
	return now.Add(-time.Hour).Format(time.RFC3339), now.Add(24 * time.Hour).Format(time.RFC3339)
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "pub.b64")
	privPath := filepath.Join(dir, "priv.b64")
	inPath := filepath.Join(dir, "bundle.json")
	outPath := filepath.Join(dir, "signed.json")

	if err := keygenCmd([]string{"--pub", pubPath, "--priv", privPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}

	// keygen must emit correctly sized, base64-decodable keys.
	if got := decodedLen(t, pubPath); got != 32 {
		t.Fatalf("public key = %d bytes, want 32", got)
	}
	if got := decodedLen(t, privPath); got != 64 {
		t.Fatalf("private key = %d bytes, want 64", got)
	}

	if err := os.WriteFile(inPath, []byte(unsignedBundle), 0o644); err != nil {
		t.Fatal(err)
	}

	issued, expires := freshWindow()
	if err := signCmd([]string{
		"--key", privPath, "--in", inPath, "--out", outPath,
		"--issued-at", issued, "--expires-at", expires,
	}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// The signed bundle must fill in content_hash and signature.
	signed := readBundle(t, outPath)
	if !strings.HasPrefix(signed["content_hash"].(string), "sha256:") {
		t.Fatalf("content_hash not set: %v", signed["content_hash"])
	}
	if !strings.HasPrefix(signed["signature"].(string), "ed25519:") {
		t.Fatalf("signature not set: %v", signed["signature"])
	}

	if err := verifyCmd([]string{"--pub", pubPath, "--in", outPath}); err != nil {
		t.Fatalf("verify of freshly signed bundle failed: %v", err)
	}
}

func TestTamperFailsVerify(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "pub.b64")
	privPath := filepath.Join(dir, "priv.b64")
	inPath := filepath.Join(dir, "bundle.json")
	outPath := filepath.Join(dir, "signed.json")

	if err := keygenCmd([]string{"--pub", pubPath, "--priv", privPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if err := os.WriteFile(inPath, []byte(unsignedBundle), 0o644); err != nil {
		t.Fatal(err)
	}
	issued, expires := freshWindow()
	if err := signCmd([]string{
		"--key", privPath, "--in", inPath, "--out", outPath,
		"--issued-at", issued, "--expires-at", expires,
	}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Tamper with drun_config after signing: content_hash no longer matches, so
	// verify must fail closed.
	tampered := readBundle(t, outPath)
	tampered["drun_config"] = map[string]any{"max_sessions": float64(99)}
	writeBundle(t, outPath, tampered)

	if err := verifyCmd([]string{"--pub", pubPath, "--in", outPath}); err == nil {
		t.Fatal("verify accepted a tampered bundle, want failure")
	}
}

func TestWrongKeyFailsVerify(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "pub.b64")
	privPath := filepath.Join(dir, "priv.b64")
	otherPubPath := filepath.Join(dir, "other_pub.b64")
	otherPrivPath := filepath.Join(dir, "other_priv.b64")
	inPath := filepath.Join(dir, "bundle.json")
	outPath := filepath.Join(dir, "signed.json")

	if err := keygenCmd([]string{"--pub", pubPath, "--priv", privPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if err := keygenCmd([]string{"--pub", otherPubPath, "--priv", otherPrivPath}); err != nil {
		t.Fatalf("keygen (other): %v", err)
	}
	if err := os.WriteFile(inPath, []byte(unsignedBundle), 0o644); err != nil {
		t.Fatal(err)
	}
	issued, expires := freshWindow()
	if err := signCmd([]string{
		"--key", privPath, "--in", inPath, "--out", outPath,
		"--issued-at", issued, "--expires-at", expires,
	}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// A valid signature from the wrong keypair must not verify.
	if err := verifyCmd([]string{"--pub", otherPubPath, "--in", outPath}); err == nil {
		t.Fatal("verify accepted a bundle under the wrong public key, want failure")
	}
}

func decodedLen(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return len(raw)
}

func readBundle(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func writeBundle(t *testing.T, path string, m map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
