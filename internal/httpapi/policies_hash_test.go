package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// sha256HexOf returns the lowercase hex sha256 of b. The policy tests use it to
// recompute the digest a handler is expected to have stored over a version's
// canonical content_json, independently of the handler's own hashing.
func sha256HexOf(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
