package policy

import (
	"crypto/ed25519"
	"time"
)

// Verify runs the fail-closed verification pipeline (spec §5) in order and
// returns the first failure:
//
//  1. structural validation (Validate),
//  2. freshness: issued_at <= now <= expires_at within DefaultClockSkew,
//  3. content_hash matches the canonical drun_config,
//  4. the Ed25519 signature verifies against pub.
//
// Rollback protection (step 4 in the spec) requires the consumer's record of
// the newest accepted policy_version and is therefore the caller's
// responsibility, not part of a single-bundle Verify.
//
// A bundle from Load has already passed step 1; Verify repeats it so it is also
// correct on bundles assembled by other means. Only if every step passes may
// the caller compile config.toml and release proxy credentials.
func (b *PolicyBundle) Verify(now time.Time, pub ed25519.PublicKey) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := b.CheckFreshness(now); err != nil {
		return err
	}
	if err := b.VerifyContentHash(); err != nil {
		return err
	}
	return b.VerifySignature(pub)
}
