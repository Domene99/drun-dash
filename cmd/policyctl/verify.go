package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Domene99/drun-dash/pkg/policy"
)

// verifyCmd loads a signed bundle and runs the full pkg/policy fail-closed
// pipeline against the supplied public key: structural validation, freshness at
// the current time, content_hash, and the Ed25519 signature. It reports an
// error (non-zero exit) on any failure.
func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pubPath := fs.String("pub", "", "path to the base64 Ed25519 public key (required)")
	inPath := fs.String("in", "", "path to the signed bundle JSON (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pubPath == "" || *inPath == "" {
		return fmt.Errorf("verify requires --pub and --in")
	}

	keyBytes, err := readKeyFile(*pubPath)
	if err != nil {
		return err
	}
	if len(keyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("public key must be %d bytes, got %d", ed25519.PublicKeySize, len(keyBytes))
	}
	pub := ed25519.PublicKey(keyBytes)

	data, err := os.ReadFile(*inPath)
	if err != nil {
		return err
	}
	b, err := policy.Load(data)
	if err != nil {
		return err
	}
	if err := b.Verify(time.Now(), pub); err != nil {
		return err
	}

	fmt.Printf("OK: %s verifies (org %s, policy %s v%d)\n", *inPath, b.OrgID, b.PolicyID, b.PolicyVersion)
	return nil
}
