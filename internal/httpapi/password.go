package httpapi

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the bcrypt work factor for password hashing (ADR-0008 §2): a
// tuned constant, never user-supplied. 12 is a deliberately slow default.
const bcryptCost = 12

// Password length bounds for a settable password (ADR-0008 §2). The 72-byte
// ceiling is bcrypt's hard input limit — bytes past it are ignored, so a longer
// password is rejected rather than silently truncated; 8 is the recommended
// floor. This is a length policy only, not a complexity regime.
const (
	minPasswordLen = 8
	maxPasswordLen = 72
)

// dummyPasswordHash is a valid bcrypt hash that verifyPassword compares against
// when a member has no password set, so a null-hash member and a wrong-password
// member take the same time — no timing oracle for account existence
// (ADR-0008 §2). Computed once at package init at bcryptCost.
var dummyPasswordHash = mustDummyHash()

func mustDummyHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("drun-dash-dummy-password-for-constant-time-compare"), bcryptCost)
	if err != nil {
		panic("httpapi: generate dummy bcrypt hash: " + err.Error())
	}
	return h
}

// validatePassword enforces the settable-password length policy (8..72 bytes),
// returning a human-readable message on failure or "" when the password is
// acceptable. It is applied on signup and invite acceptance, before hashing.
func validatePassword(pw string) string {
	switch {
	case len(pw) < minPasswordLen:
		return fmt.Sprintf("password must be at least %d characters", minPasswordLen)
	case len(pw) > maxPasswordLen:
		return fmt.Sprintf("password must be at most %d bytes", maxPasswordLen)
	default:
		return ""
	}
}

// hashPassword bcrypt-hashes pw at bcryptCost and returns the encoded hash.
// Callers must validatePassword first (bcrypt itself errors past 72 bytes).
func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("httpapi: hash password: %w", err)
	}
	return string(h), nil
}

// verifyPassword reports whether pw matches the member's stored hash. When hash
// is nil (no password set), it still performs a bcrypt compare against the fixed
// dummy hash so the elapsed time matches a wrong-password attempt on a real
// hash, then reports false — closing the account-existence timing oracle
// (ADR-0008 §2). The compare is constant-time within bcrypt.
func verifyPassword(hash *string, pw string) bool {
	if hash == nil {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(pw))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(*hash), []byte(pw)) == nil
}
