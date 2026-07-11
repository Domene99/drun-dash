package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
)

// keygenCmd generates a fresh Ed25519 keypair and writes the base64-encoded
// public (32-byte) and private (64-byte) keys to the requested destinations.
// With no --pub/--priv flag the corresponding key is printed to stdout instead
// of a file.
func keygenCmd(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	pubPath := fs.String("pub", "", "write the public key (base64, 32 bytes) to this file; default stdout")
	privPath := fs.String("priv", "", "write the private key (base64, 64 bytes) to this file; default stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	if err := writeKey(*pubPath, base64.StdEncoding.EncodeToString(pub), "public", 0o644); err != nil {
		return err
	}
	if err := writeKey(*privPath, base64.StdEncoding.EncodeToString(priv), "private", 0o600); err != nil {
		return err
	}
	return nil
}

// writeKey writes the base64 key to path with the given permissions, appending
// a trailing newline. When path is empty the key is printed to stdout, labeled
// by kind, so a caller can capture either half without a file.
func writeKey(path, b64, kind string, perm os.FileMode) error {
	if path == "" {
		fmt.Printf("%s: %s\n", kind, b64)
		return nil
	}
	if err := os.WriteFile(path, []byte(b64+"\n"), perm); err != nil {
		return fmt.Errorf("write %s key %s: %w", kind, path, err)
	}
	return nil
}
