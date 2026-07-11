package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Domene99/drun-dash/pkg/policy"
)

// bundleWire is the on-the-wire JSON shape policyctl reads and writes. drun_config
// is kept as raw bytes so the operator-authored config passes through signing
// untouched: content_hash is computed over those exact bytes and the config is
// re-emitted without this tool ever modeling (and thus dropping) its fields.
type bundleWire struct {
	SchemaVersion string          `json:"schema_version"`
	OrgID         string          `json:"org_id"`
	PolicyID      string          `json:"policy_id"`
	PolicyVersion int             `json:"policy_version"`
	DrunConfig    json.RawMessage `json:"drun_config"`
	IssuedAt      string          `json:"issued_at"`
	ExpiresAt     string          `json:"expires_at"`
	ContentHash   string          `json:"content_hash"`
	Signature     string          `json:"signature"`
}

// signCmd fills in a bundle's content_hash and Ed25519 signature. It reads an
// input bundle (whose content_hash/signature may be empty placeholders),
// recomputes content_hash over the exact drun_config bytes, signs the canonical
// signing input, writes the assembled bundle to --out, and re-loads and fully
// verifies it before returning so a broken bundle never reaches disk unnoticed.
func signCmd(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "path to the base64 Ed25519 private key (required)")
	inPath := fs.String("in", "", "path to the input bundle JSON (required)")
	outPath := fs.String("out", "", "path to write the signed bundle JSON (required)")
	issuedAt := fs.String("issued-at", "", "override issued_at (RFC 3339 UTC)")
	expiresAt := fs.String("expires-at", "", "override expires_at (RFC 3339 UTC)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || *inPath == "" || *outPath == "" {
		return fmt.Errorf("sign requires --key, --in, and --out")
	}

	keyBytes, err := readKeyFile(*keyPath)
	if err != nil {
		return err
	}
	if len(keyBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(keyBytes))
	}
	priv := ed25519.PrivateKey(keyBytes)

	inData, err := os.ReadFile(*inPath)
	if err != nil {
		return err
	}
	var w bundleWire
	if err := json.Unmarshal(inData, &w); err != nil {
		return fmt.Errorf("parse input bundle: %w", err)
	}
	if len(w.DrunConfig) == 0 {
		return fmt.Errorf("input bundle is missing drun_config")
	}

	if *issuedAt != "" {
		w.IssuedAt = *issuedAt
	}
	if *expiresAt != "" {
		w.ExpiresAt = *expiresAt
	}

	contentHash, err := policy.ComputeContentHash(w.DrunConfig)
	if err != nil {
		return fmt.Errorf("compute content hash: %w", err)
	}
	w.ContentHash = contentHash

	// SigningInput reads only the exported scalar fields, so an in-memory
	// PolicyBundle carrying the values above reproduces exactly what pkg/policy
	// will verify.
	b := policy.PolicyBundle{
		SchemaVersion: w.SchemaVersion,
		OrgID:         w.OrgID,
		PolicyID:      w.PolicyID,
		PolicyVersion: w.PolicyVersion,
		IssuedAt:      w.IssuedAt,
		ExpiresAt:     w.ExpiresAt,
		ContentHash:   w.ContentHash,
	}
	sig := ed25519.Sign(priv, b.SigningInput())
	w.Signature = "ed25519:" + base64.StdEncoding.EncodeToString(sig)

	out, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.WriteFile(*outPath, out, 0o644); err != nil {
		return fmt.Errorf("write signed bundle: %w", err)
	}

	// Round-trip: the freshly written bundle must load and fully verify against
	// the public half of the signing key before we report success.
	pub := priv.Public().(ed25519.PublicKey)
	signed, err := policy.Load(out)
	if err != nil {
		return fmt.Errorf("round-trip load of signed bundle: %w", err)
	}
	if err := signed.Verify(time.Now(), pub); err != nil {
		return fmt.Errorf("round-trip verify of signed bundle: %w", err)
	}

	fmt.Printf("signed %s -> %s (%s, %s %s v%d)\n", *inPath, *outPath, b.ContentHash, b.OrgID, b.PolicyID, b.PolicyVersion)
	return nil
}
