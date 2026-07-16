package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/Domene99/drun-dash/internal/store"
)

// dbBackend is the namespace [DBVault] serves: refs of the form
//
//	vault://db/<uuid>
//
// where <uuid> is the id of a row in the secrets table (migration 000009).
const dbBackend = "db"

// dbRefPrefix is the fixed prefix of every DB-vault ref.
const dbRefPrefix = refScheme + dbBackend + "/"

// masterKeySize is the required master-key length: AES-256 takes a 32-byte key.
const masterKeySize = 32

// ErrKeyNotConfigured means the DB vault has no master key, so it can neither
// seal new secrets nor open existing ones. It is returned when DRUNDASH_VAULT_KEY
// is unset (or was malformed at boot): the service still runs, but db-backed
// secrets are unavailable until the operator sets the key. Callers compare with
// [errors.Is]; the API surfaces it as a clear 400 telling the operator to set
// the variable. It never carries any secret material.
var ErrKeyNotConfigured = errors.New("vault: DRUNDASH_VAULT_KEY is not set; cannot seal or open db-backed secrets")

// SecretStore is the persistence seam [DBVault] needs: the CRUD surface over the
// secrets table (an *[store.SecretStore] satisfies it). Naming it as an
// interface lets tests inject an in-memory fake without a database, matching the
// store-seam style used elsewhere.
type SecretStore interface {
	Create(ctx context.Context, p store.CreateSecretParams) (store.Secret, error)
	GetByID(ctx context.Context, id string) (store.Secret, error)
	Update(ctx context.Context, id string, ciphertext, nonce []byte) error
	Delete(ctx context.Context, id string) error
}

// DBVault is the writable, encrypted-at-rest [Vault] backend (ADR-0009). It
// seals provider credentials with AES-256-GCM under a process-wide master key
// (DRUNDASH_VAULT_KEY) and stores only the ciphertext + nonce in the secrets
// table, referenced by a vault://db/<uuid> ref. The raw credential is never
// persisted or logged; it exists only transiently in memory during a seal or
// open, and is returned wrapped in the redacting [Secret].
//
// A DBVault built without a master key is valid but disabled: every seal/open
// returns [ErrKeyNotConfigured] so the service still boots and dev-backed
// (vault://dev/*) secrets keep working through the [CompositeVault]. DBVault is
// safe for concurrent use (the AEAD is stateless and each call draws a fresh
// nonce).
type DBVault struct {
	store SecretStore
	// aead is the AES-256-GCM cipher built from the master key, or nil when no
	// key is configured (the disabled state).
	aead cipher.AEAD
}

// DBVault implements [Vault].
var _ Vault = DBVault{}

// NewDBVault builds a [DBVault] over store, keyed by masterKey. masterKey must
// be exactly 32 bytes (AES-256); a nil/empty key yields a disabled vault whose
// seal/open operations return [ErrKeyNotConfigured] (so a service with no
// DRUNDASH_VAULT_KEY still boots and serves dev-backed secrets). A key of any
// other non-zero length is an operator error and returns an error.
func NewDBVault(store SecretStore, masterKey []byte) (DBVault, error) {
	if len(masterKey) == 0 {
		return DBVault{store: store}, nil
	}
	if len(masterKey) != masterKeySize {
		return DBVault{}, fmt.Errorf("vault: master key must be %d bytes, got %d", masterKeySize, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return DBVault{}, fmt.Errorf("vault: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return DBVault{}, fmt.Errorf("vault: build gcm: %w", err)
	}
	return DBVault{store: store, aead: aead}, nil
}

// Resolve implements [Vault]. It parses secretRef against "vault://db/<uuid>",
// loads the sealed row, and AES-256-GCM-opens it, returning the plaintext
// credential in a redacting [Secret].
//
//   - A ref that does not match the db grammar returns [ErrInvalidRef].
//   - A well-formed ref with no configured master key returns [ErrKeyNotConfigured].
//   - A well-formed ref whose row is missing returns [ErrNotFound].
//
// The returned error never contains the credential value or the master key.
func (v DBVault) Resolve(ctx context.Context, secretRef string) (Secret, error) {
	id, err := parseDBRef(secretRef)
	if err != nil {
		return Secret{}, err
	}
	if v.aead == nil {
		return Secret{}, ErrKeyNotConfigured
	}
	row, err := v.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Secret{}, fmt.Errorf("%w: %s", ErrNotFound, secretRef)
		}
		return Secret{}, fmt.Errorf("vault: load secret: %w", err)
	}
	plaintext, err := v.aead.Open(nil, row.Nonce, row.Ciphertext, nil)
	if err != nil {
		// A GCM open failure means the ciphertext does not authenticate under
		// this key (wrong/rotated key, or tampering). The error carries no
		// plaintext.
		return Secret{}, fmt.Errorf("vault: open sealed secret %s: %w", secretRef, err)
	}
	return Secret{Value: string(plaintext)}, nil
}

// Seal encrypts plaintext under the master key with a fresh random nonce,
// persists the ciphertext for orgID, and returns the "vault://db/<uuid>" ref
// that resolves back to it. It returns [ErrKeyNotConfigured] when no master key
// is set. plaintext is never logged and never leaves memory unencrypted.
func (v DBVault) Seal(ctx context.Context, orgID, plaintext string) (string, error) {
	if v.aead == nil {
		return "", ErrKeyNotConfigured
	}
	nonce, ciphertext, err := v.seal(plaintext)
	if err != nil {
		return "", err
	}
	row, err := v.store.Create(ctx, store.CreateSecretParams{OrgID: orgID, Ciphertext: ciphertext, Nonce: nonce})
	if err != nil {
		return "", fmt.Errorf("vault: store sealed secret: %w", err)
	}
	return dbRefPrefix + row.ID, nil
}

// Reseal re-encrypts plaintext under a fresh nonce and overwrites the secret an
// existing "vault://db/<uuid>" ref points at (used to rotate a credential in
// place without changing the endpoint's secret_ref). A malformed ref returns
// [ErrInvalidRef], no master key returns [ErrKeyNotConfigured], and a missing
// row returns [ErrNotFound].
func (v DBVault) Reseal(ctx context.Context, secretRef, plaintext string) error {
	id, err := parseDBRef(secretRef)
	if err != nil {
		return err
	}
	if v.aead == nil {
		return ErrKeyNotConfigured
	}
	nonce, ciphertext, err := v.seal(plaintext)
	if err != nil {
		return err
	}
	if err := v.store.Update(ctx, id, ciphertext, nonce); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrNotFound, secretRef)
		}
		return fmt.Errorf("vault: reseal secret %s: %w", secretRef, err)
	}
	return nil
}

// Delete removes the sealed secret an existing "vault://db/<uuid>" ref points
// at. It does not require a master key (the ciphertext is discarded, not
// opened). A malformed ref returns [ErrInvalidRef]; a missing row returns
// [ErrNotFound].
func (v DBVault) Delete(ctx context.Context, secretRef string) error {
	id, err := parseDBRef(secretRef)
	if err != nil {
		return err
	}
	if err := v.store.Delete(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrNotFound, secretRef)
		}
		return fmt.Errorf("vault: delete secret %s: %w", secretRef, err)
	}
	return nil
}

// seal draws a fresh random nonce and AES-256-GCM-seals plaintext, returning the
// nonce and ciphertext. The caller has already checked v.aead is non-nil.
func (v DBVault) seal(plaintext string) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("vault: generate nonce: %w", err)
	}
	ciphertext = v.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return nonce, ciphertext, nil
}

// IsDBRef reports whether secretRef names the db backend (vault://db/*). The
// endpoints handler uses it to decide whether a stored secret_ref has a backing
// secrets row to reseal or delete.
func IsDBRef(secretRef string) bool {
	_, err := parseDBRef(secretRef)
	return err == nil
}

// parseDBRef validates secretRef against "vault://db/<uuid>" and returns the
// bare <uuid>. It is total: every input yields either an id or [ErrInvalidRef],
// and it never panics. The id is validated against the canonical UUID shape so
// a malformed ref is rejected here rather than surfacing as a database
// type error downstream.
func parseDBRef(secretRef string) (string, error) {
	id, ok := cutPrefix(secretRef, dbRefPrefix)
	if !ok {
		return "", fmt.Errorf("%w: %q (want %s<uuid>)", ErrInvalidRef, secretRef, dbRefPrefix)
	}
	if !isUUID(id) {
		return "", fmt.Errorf("%w: %q (id must be a uuid)", ErrInvalidRef, secretRef)
	}
	return id, nil
}

// cutPrefix is strings.CutPrefix, inlined to keep this file's imports minimal.
func cutPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}

// isUUID reports whether s has the canonical 8-4-4-4-12 hex UUID shape. It is a
// cheap syntactic guard, not a version/variant check.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHex(r) {
				return false
			}
		}
	}
	return true
}

func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
