package policy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ComputeContentHash returns "sha256:" + lowercase-hex SHA-256 of the canonical
// JSON serialization of drunConfig (spec §3). drunConfig must be the JSON bytes
// of the drun_config object.
func ComputeContentHash(drunConfig json.RawMessage) (string, error) {
	canon, err := canonicalJSON(drunConfig)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyContentHash recomputes the canonical hash of the bundle's drun_config
// and compares it to ContentHash, returning ErrContentHashMismatch on any
// difference (spec §5, step 5).
//
// When the bundle was produced by Load, the hash is computed over the exact
// drun_config bytes that were received. For a bundle assembled in memory
// (rawDrunConfig empty), it falls back to the canonical serialization of the
// DrunConfig struct via CanonicalJSON.
func (b *PolicyBundle) VerifyContentHash() error {
	var (
		canon []byte
		err   error
	)
	if len(b.rawDrunConfig) > 0 {
		canon, err = canonicalJSON(b.rawDrunConfig)
	} else {
		canon, err = b.DrunConfig.CanonicalJSON()
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canon)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != b.ContentHash {
		return fmt.Errorf("%w: recomputed %s, bundle declares %s", ErrContentHashMismatch, got, b.ContentHash)
	}
	return nil
}

// CanonicalJSON returns the canonical JSON serialization of the config's
// present fields (spec §3): keys sorted lexicographically, no insignificant
// whitespace, array order preserved. A nil slice or nil pointer is omitted; a
// non-nil empty slice is emitted as []. This is the byte string that
// content_hash digests.
func (c *DrunConfig) CanonicalJSON() ([]byte, error) {
	m := map[string]any{}
	putSlice(m, "domain_allowlist", c.DomainAllowlist)
	putSlice(m, "mount_allowlist", c.MountAllowlist)
	putSlice(m, "mount_overlay_paths", c.MountOverlayPaths)
	putSlice(m, "env_allowlist", c.EnvAllowlist)
	putSlice(m, "bash_command_denylist", c.BashCommandDenylist)
	putSlice(m, "bash_command_allowlist", c.BashCommandAllowlist)
	putInt(m, "bash_timeout_ms", c.BashTimeoutMs)
	putInt(m, "fetch_timeout_ms", c.FetchTimeoutMs)
	putInt(m, "connect_timeout_ms", c.ConnectTimeoutMs)
	putInt(m, "max_workspace_mb", c.MaxWorkspaceMb)
	putInt(m, "max_sessions", c.MaxSessions)
	putInt(m, "max_checkpoints", c.MaxCheckpoints)
	putInt(m, "session_idle_timeout_secs", c.SessionIdleTimeoutSecs)

	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(raw)
}

func putSlice(m map[string]any, key string, v []string) {
	if v != nil {
		m[key] = v
	}
}

func putInt(m map[string]any, key string, v *int64) {
	if v != nil {
		m[key] = *v
	}
}

// canonicalJSON re-serializes arbitrary JSON into the canonical form required
// by spec §3 (JCS essentials): object keys sorted lexicographically and
// recursively, separators "," and ":" with no other whitespace, UTF-8, array
// order preserved, and integers emitted verbatim (numbers are decoded with
// json.Number so large integer values are not reformatted through float64).
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("policy: canonicalize drun_config: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("policy: canonicalize drun_config: unexpected trailing data")
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(t.String())
	case string:
		writeCanonicalString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("policy: canonicalize: unsupported JSON type %T", v)
	}
	return nil
}

// writeCanonicalString writes s as a JSON string using the minimal escaping of
// RFC 8785 §3.2.2.2 (the two mandatory escapes, the five short escapes, and
// \u00XX for the remaining control characters); every other code point,
// including non-ASCII, is emitted literally in UTF-8. This matches the Python
// json.dumps(ensure_ascii=False) reference the fixtures were generated with.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// SigningInput returns the ASCII signing input for the bundle (spec §4): a
// fixed domain-separation tag followed by the identity, freshness, and
// content-hash fields, one per line, with no trailing newline. Because it
// includes content_hash (which covers the canonical drun_config), a valid
// signature over this input authenticates the entire bundle.
func (b *PolicyBundle) SigningInput() []byte {
	return []byte(strings.Join([]string{
		"drun-policy-bundle/v1",
		b.SchemaVersion,
		b.OrgID,
		b.PolicyID,
		strconv.Itoa(b.PolicyVersion),
		b.IssuedAt,
		b.ExpiresAt,
		b.ContentHash,
	}, "\n"))
}

// Signature returns the raw 64-byte Ed25519 signature decoded from the bundle's
// "ed25519:"-prefixed base64 field, or an error if the prefix, base64, or
// length is wrong.
func (b *PolicyBundle) signatureBytes() ([]byte, error) {
	raw, ok := strings.CutPrefix(b.Signature, "ed25519:")
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrBadSignatureFormat, b.Signature)
	}
	sig, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: base64: %v", ErrBadSignatureFormat, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrBadSignatureFormat, len(sig), ed25519.SignatureSize)
	}
	return sig, nil
}

// VerifySignature verifies the bundle's Ed25519 signature against pub over
// SigningInput (spec §5, step 6). It returns ErrBadPublicKey if pub is the
// wrong size, ErrBadSignatureFormat if the signature field is malformed, and
// ErrSignatureVerify if the signature does not verify.
//
// The shipped test fixtures carry a non-verifying placeholder signature (spec
// §4); signing keys arrive in a later batch, so this is exercised against a
// locally generated keypair, not the fixtures.
func (b *PolicyBundle) VerifySignature(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrBadPublicKey, len(pub), ed25519.PublicKeySize)
	}
	sig, err := b.signatureBytes()
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, b.SigningInput(), sig) {
		return ErrSignatureVerify
	}
	return nil
}
