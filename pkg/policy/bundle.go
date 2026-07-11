// Package policy implements loading, structural validation, content-hash
// verification, and drun config.toml compilation for drun-dash signed policy
// bundles.
//
// A policy bundle is a JSON document (see docs/policy/DRUN_POLICY_BUNDLE.md)
// that the control plane distributes to fleets of drun sandboxes. It carries
// the authenticity and freshness metadata the control plane needs plus a
// drun_config object that compiles one-to-one into a drun ~/.drun/config.toml.
//
// This package implements everything except signing-key management:
//
//   - Load parses a bundle, rejecting unknown top-level fields, and runs
//     structural validation.
//   - PolicyBundle.CheckFreshness enforces the issued_at/expires_at window.
//   - PolicyBundle.VerifyContentHash recomputes the canonical hash of
//     drun_config and compares it to content_hash.
//   - PolicyBundle.VerifySignature verifies the Ed25519 signature against a
//     caller-supplied public key.
//   - DrunConfig.ToTOML compiles the config to config.toml bytes.
//
// Signing, key distribution, and rollback-protection state live in later
// batches; the shipped fixtures carry a non-verifying placeholder signature, so
// VerifySignature is exercised with a locally generated key, not the fixtures.
package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SchemaMajor is the bundle-format major version this package understands. A
// bundle whose schema_version major differs is rejected (spec §1).
const SchemaMajor = 1

// PolicyBundle is a parsed drun policy bundle (spec §1).
//
// Construct one with Load; the zero value is not meaningful. rawDrunConfig is
// captured from the input so VerifyContentHash can hash the exact bytes that
// were received rather than a re-serialization of DrunConfig.
type PolicyBundle struct {
	// SchemaVersion is the bundle-format version, e.g. "1.0".
	SchemaVersion string `json:"schema_version"`
	// OrgID is the stable identifier of the owning organization.
	OrgID string `json:"org_id"`
	// PolicyID is the stable identifier of the policy within the org.
	PolicyID string `json:"policy_id"`
	// PolicyVersion is a monotonically increasing version for this PolicyID,
	// used for rollback protection by consumers.
	PolicyVersion int `json:"policy_version"`
	// DrunConfig is the compiled drun configuration (spec §2).
	DrunConfig DrunConfig `json:"drun_config"`
	// IssuedAt is the RFC 3339 UTC timestamp when the bundle was signed.
	IssuedAt string `json:"issued_at"`
	// ExpiresAt is the RFC 3339 UTC timestamp after which the bundle is
	// invalid; it must be strictly after IssuedAt.
	ExpiresAt string `json:"expires_at"`
	// ContentHash is "sha256:" + hex SHA-256 of the canonical drun_config JSON.
	ContentHash string `json:"content_hash"`
	// Signature is "ed25519:" + base64 Ed25519 signature over the signing
	// input (spec §4).
	Signature string `json:"signature"`

	// rawDrunConfig is the exact JSON bytes of the drun_config object as
	// received. content_hash is computed over the canonicalization of these
	// bytes (see VerifyContentHash), so fields this package does not model and
	// insignificant formatting cannot alter the verified hash.
	rawDrunConfig json.RawMessage
}

// DrunConfig holds the fields that map one-to-one onto keys in drun's
// ~/.drun/config.toml (spec §2). Every field is optional: a nil slice or nil
// pointer means "field omitted, use the drun default". A non-nil but empty
// slice means "present and explicitly empty" (e.g. bash_command_allowlist = []
// selects denylist mode) and is preserved through both hashing and TOML
// compilation.
type DrunConfig struct {
	DomainAllowlist        []string `json:"domain_allowlist,omitempty"`
	MountAllowlist         []string `json:"mount_allowlist,omitempty"`
	MountOverlayPaths      []string `json:"mount_overlay_paths,omitempty"`
	EnvAllowlist           []string `json:"env_allowlist,omitempty"`
	BashCommandDenylist    []string `json:"bash_command_denylist,omitempty"`
	BashCommandAllowlist   []string `json:"bash_command_allowlist,omitempty"`
	BashTimeoutMs          *int64   `json:"bash_timeout_ms,omitempty"`
	FetchTimeoutMs         *int64   `json:"fetch_timeout_ms,omitempty"`
	ConnectTimeoutMs       *int64   `json:"connect_timeout_ms,omitempty"`
	MaxWorkspaceMb         *int64   `json:"max_workspace_mb,omitempty"`
	MaxSessions            *int64   `json:"max_sessions,omitempty"`
	MaxCheckpoints         *int64   `json:"max_checkpoints,omitempty"`
	SessionIdleTimeoutSecs *int64   `json:"session_idle_timeout_secs,omitempty"`
}

// bundleWire is the on-the-wire shape used for strict decoding. Required
// scalar fields use pointers/RawMessage so Load can distinguish "absent" from
// a legitimate zero value.
type bundleWire struct {
	SchemaVersion string          `json:"schema_version"`
	OrgID         string          `json:"org_id"`
	PolicyID      string          `json:"policy_id"`
	PolicyVersion *int            `json:"policy_version"`
	DrunConfig    json.RawMessage `json:"drun_config"`
	IssuedAt      string          `json:"issued_at"`
	ExpiresAt     string          `json:"expires_at"`
	ContentHash   string          `json:"content_hash"`
	Signature     string          `json:"signature"`
}

// Load parses and structurally validates a policy bundle from JSON.
//
// It rejects unknown top-level fields and unknown drun_config fields (extra
// fields are treated as tampering, spec §1), requires every top-level field to
// be present, and runs Validate for format checks (schema major, RFC 3339
// timestamps, expires_at strictly after issued_at, hash/signature prefixes).
//
// Load does NOT check freshness or verify the content hash or signature —
// those need a clock and a public key and are separate steps in the
// fail-closed pipeline (spec §5). Use CheckFreshness, VerifyContentHash, and
// VerifySignature, or the Verify convenience, for the remaining checks.
func Load(data []byte) (*PolicyBundle, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var w bundleWire
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("policy: decode bundle: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("policy: unexpected trailing data after bundle JSON")
	}

	if missing := firstMissing(&w); missing != "" {
		return nil, fmt.Errorf("%w: %s", ErrMissingField, missing)
	}

	b := &PolicyBundle{
		SchemaVersion: w.SchemaVersion,
		OrgID:         w.OrgID,
		PolicyID:      w.PolicyID,
		PolicyVersion: *w.PolicyVersion,
		IssuedAt:      w.IssuedAt,
		ExpiresAt:     w.ExpiresAt,
		ContentHash:   w.ContentHash,
		Signature:     w.Signature,
		rawDrunConfig: append(json.RawMessage(nil), w.DrunConfig...),
	}

	cfgDec := json.NewDecoder(bytes.NewReader(w.DrunConfig))
	cfgDec.DisallowUnknownFields()
	if err := cfgDec.Decode(&b.DrunConfig); err != nil {
		return nil, fmt.Errorf("policy: decode drun_config: %w", err)
	}

	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// firstMissing returns the name of the first required top-level field that is
// absent, or "" if all are present. drun_config is considered missing if it is
// absent or JSON null.
func firstMissing(w *bundleWire) string {
	switch {
	case w.SchemaVersion == "":
		return "schema_version"
	case w.OrgID == "":
		return "org_id"
	case w.PolicyID == "":
		return "policy_id"
	case w.PolicyVersion == nil:
		return "policy_version"
	case len(w.DrunConfig) == 0 || string(w.DrunConfig) == "null":
		return "drun_config"
	case w.IssuedAt == "":
		return "issued_at"
	case w.ExpiresAt == "":
		return "expires_at"
	case w.ContentHash == "":
		return "content_hash"
	case w.Signature == "":
		return "signature"
	default:
		return ""
	}
}
