package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Validation sentinel errors. Callers can match specific failure modes with
// errors.Is.
var (
	// ErrMissingField indicates a required field was absent. The error message
	// names the field.
	ErrMissingField = errors.New("policy: missing required field")
	// ErrUnsupportedSchema indicates schema_version's major version is not
	// understood.
	ErrUnsupportedSchema = errors.New("policy: unsupported schema_version")
	// ErrInvalidPolicyVersion indicates policy_version is not a positive
	// integer.
	ErrInvalidPolicyVersion = errors.New("policy: policy_version must be >= 1")
	// ErrInvalidTimestamp indicates issued_at or expires_at is not RFC 3339.
	ErrInvalidTimestamp = errors.New("policy: timestamp must be RFC3339")
	// ErrExpiryBeforeIssue indicates expires_at is not strictly after
	// issued_at.
	ErrExpiryBeforeIssue = errors.New("policy: expires_at must be after issued_at")
	// ErrBadContentHashFormat indicates content_hash lacks the "sha256:"
	// prefix.
	ErrBadContentHashFormat = errors.New("policy: content_hash must be sha256:<hex>")
	// ErrBadSignatureFormat indicates signature lacks the "ed25519:" prefix or
	// is not valid base64 of a 64-byte signature.
	ErrBadSignatureFormat = errors.New("policy: signature must be ed25519:<base64>")

	// ErrNotYetValid indicates the current time is before issued_at (minus the
	// clock-skew allowance).
	ErrNotYetValid = errors.New("policy: bundle not yet valid")
	// ErrExpired indicates the current time is after expires_at (plus the
	// clock-skew allowance).
	ErrExpired = errors.New("policy: bundle expired")

	// ErrContentHashMismatch indicates the recomputed canonical hash of
	// drun_config does not match content_hash (tampering).
	ErrContentHashMismatch = errors.New("policy: content_hash mismatch")

	// ErrBadPublicKey indicates the supplied Ed25519 public key is the wrong
	// size.
	ErrBadPublicKey = errors.New("policy: invalid ed25519 public key")
	// ErrSignatureVerify indicates the Ed25519 signature did not verify against
	// the public key and signing input.
	ErrSignatureVerify = errors.New("policy: signature verification failed")
)

// DefaultClockSkew is the allowance applied on each side of the validity window
// in CheckFreshness, absorbing minor clock drift between signer and consumer
// (spec §5, step 3).
const DefaultClockSkew = 60 * time.Second

// Validate performs the time-independent structural checks of the fail-closed
// pipeline (spec §5, steps 1–2 plus format checks): the schema major version is
// understood, policy_version is positive, issued_at and expires_at are RFC 3339
// with expires_at strictly after issued_at, and content_hash/signature carry
// their required prefixes.
//
// Load calls Validate, so a bundle returned by Load is already structurally
// valid. Validate does not consult a clock, recompute the hash, or verify the
// signature.
func (b *PolicyBundle) Validate() error {
	major, err := schemaMajor(b.SchemaVersion)
	if err != nil {
		return err
	}
	if major != SchemaMajor {
		return fmt.Errorf("%w: %q (understood major version %d)", ErrUnsupportedSchema, b.SchemaVersion, SchemaMajor)
	}

	if b.PolicyVersion < 1 {
		return fmt.Errorf("%w: got %d", ErrInvalidPolicyVersion, b.PolicyVersion)
	}

	issued, err := time.Parse(time.RFC3339, b.IssuedAt)
	if err != nil {
		return fmt.Errorf("%w: issued_at %q", ErrInvalidTimestamp, b.IssuedAt)
	}
	expires, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		return fmt.Errorf("%w: expires_at %q", ErrInvalidTimestamp, b.ExpiresAt)
	}
	if !expires.After(issued) {
		return fmt.Errorf("%w: expires_at %q, issued_at %q", ErrExpiryBeforeIssue, b.ExpiresAt, b.IssuedAt)
	}

	if !strings.HasPrefix(b.ContentHash, "sha256:") {
		return fmt.Errorf("%w: %q", ErrBadContentHashFormat, b.ContentHash)
	}
	if !strings.HasPrefix(b.Signature, "ed25519:") {
		return fmt.Errorf("%w: %q", ErrBadSignatureFormat, b.Signature)
	}

	return nil
}

// CheckFreshness reports whether now lies within [issued_at, expires_at],
// widened by DefaultClockSkew on each side (spec §5, step 3). It returns
// ErrNotYetValid if now is before the window and ErrExpired if after.
func (b *PolicyBundle) CheckFreshness(now time.Time) error {
	return b.checkFreshness(now, DefaultClockSkew)
}

func (b *PolicyBundle) checkFreshness(now time.Time, skew time.Duration) error {
	issued, err := time.Parse(time.RFC3339, b.IssuedAt)
	if err != nil {
		return fmt.Errorf("%w: issued_at %q", ErrInvalidTimestamp, b.IssuedAt)
	}
	expires, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		return fmt.Errorf("%w: expires_at %q", ErrInvalidTimestamp, b.ExpiresAt)
	}
	if now.Before(issued.Add(-skew)) {
		return fmt.Errorf("%w: now %s before issued_at %s", ErrNotYetValid, now.UTC().Format(time.RFC3339), b.IssuedAt)
	}
	if now.After(expires.Add(skew)) {
		return fmt.Errorf("%w: now %s after expires_at %s", ErrExpired, now.UTC().Format(time.RFC3339), b.ExpiresAt)
	}
	return nil
}

// schemaMajor extracts the integer major version from a "major.minor" schema
// string (e.g. "1.0" -> 1).
func schemaMajor(v string) (int, error) {
	maj := v
	if i := strings.IndexByte(v, '.'); i >= 0 {
		maj = v[:i]
	}
	n, err := strconv.Atoi(maj)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrUnsupportedSchema, v)
	}
	return n, nil
}
