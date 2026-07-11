// Command policyctl generates Ed25519 keypairs, signs drun policy bundles, and
// verifies signed bundles. It is the operator-side counterpart to the
// verification-only github.com/Domene99/drun-dash/pkg/policy library: policyctl
// produces the signed bundles that the control plane later verifies and
// distributes to fleets of drun sandboxes.
//
// Usage:
//
//	policyctl keygen [--pub <file>] [--priv <file>]
//	policyctl sign   --key <priv> --in <bundle.json> --out <signed.json> [--issued-at <ts>] [--expires-at <ts>]
//	policyctl verify --pub <pub> --in <signed.json>
//
// Keys are base64-encoded raw Ed25519 bytes (32-byte public key, 64-byte
// private key). Timestamps are RFC 3339 UTC, e.g. 2026-07-11T00:00:00Z.
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygenCmd(os.Args[2:])
	case "sign":
		err = signCmd(os.Args[2:])
	case "verify":
		err = verifyCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "policyctl: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "policyctl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `policyctl — Ed25519 signing tool for drun policy bundles

Usage:
  policyctl keygen [--pub <file>] [--priv <file>]
  policyctl sign   --key <priv> --in <bundle.json> --out <signed.json> [--issued-at <ts>] [--expires-at <ts>]
  policyctl verify --pub <pub> --in <signed.json>

Keys are base64-encoded raw Ed25519 bytes (32-byte public key, 64-byte private
key). Timestamps are RFC 3339 UTC, e.g. 2026-07-11T00:00:00Z.
`)
}

// readKeyFile reads a base64-encoded Ed25519 key from path and returns the
// decoded raw bytes. Surrounding whitespace, including the trailing newline
// keygen writes, is ignored.
func readKeyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("decode base64 key %s: %w", path, err)
	}
	return raw, nil
}
