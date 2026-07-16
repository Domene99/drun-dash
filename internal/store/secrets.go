package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Secret is a row of the secrets table (migration 000009): an AES-256-GCM
// ciphertext and the nonce it was sealed with, scoped to an organization. It is
// the encrypted-at-rest backing store for the DB vault backend (ADR-0009); the
// raw provider credential is never stored here — only sealed bytes — and the
// master key that opens them lives only in the process environment.
//
// This Secret (a ciphertext row) is distinct from vault.Secret (a resolved,
// redacting plaintext credential). The vault package seals plaintext into these
// rows and opens them back; nothing in this package handles the master key or
// the plaintext.
type Secret struct {
	ID         string    // id (uuid)
	OrgID      string    // org_id → organizations.id
	Ciphertext []byte    // ciphertext (bytea) — AES-256-GCM sealed bytes
	Nonce      []byte    // nonce (bytea) — the per-secret GCM nonce
	CreatedAt  time.Time // created_at
	UpdatedAt  time.Time // updated_at
}

// SecretStore reads and writes the secrets table.
type SecretStore struct {
	db Querier
}

// NewSecretStore returns a SecretStore backed by db.
func NewSecretStore(db Querier) *SecretStore {
	return &SecretStore{db: db}
}

const secretColumns = "id, org_id, ciphertext, nonce, created_at, updated_at"

func scanSecret(row pgx.Row) (Secret, error) {
	var s Secret
	err := row.Scan(&s.ID, &s.OrgID, &s.Ciphertext, &s.Nonce, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// CreateSecretParams carries the fields for [SecretStore.Create]. The database
// assigns id, created_at, and updated_at.
type CreateSecretParams struct {
	OrgID      string
	Ciphertext []byte
	Nonce      []byte
}

// Create inserts a sealed secret and returns the stored row.
func (s *SecretStore) Create(ctx context.Context, p CreateSecretParams) (Secret, error) {
	const q = `INSERT INTO secrets (org_id, ciphertext, nonce)
		VALUES ($1, $2, $3) RETURNING ` + secretColumns
	sec, err := scanSecret(s.db.QueryRow(ctx, q, p.OrgID, p.Ciphertext, p.Nonce))
	if err != nil {
		return Secret{}, fmt.Errorf("store: create secret for org %q: %w", p.OrgID, mapError(err))
	}
	return sec, nil
}

// GetByID returns the secret with the given id, or [ErrNotFound].
func (s *SecretStore) GetByID(ctx context.Context, id string) (Secret, error) {
	const q = `SELECT ` + secretColumns + ` FROM secrets WHERE id = $1`
	sec, err := scanSecret(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Secret{}, fmt.Errorf("store: secret by id %q: %w", id, mapError(err))
	}
	return sec, nil
}

// Update replaces the ciphertext and nonce of an existing secret (used for key
// rotation / credential rotation). It returns [ErrNotFound] if no such secret
// exists and stamps updated_at.
func (s *SecretStore) Update(ctx context.Context, id string, ciphertext, nonce []byte) error {
	const q = `UPDATE secrets SET ciphertext = $2, nonce = $3, updated_at = now() WHERE id = $1`
	tag, err := s.db.Exec(ctx, q, id, ciphertext, nonce)
	if err != nil {
		return fmt.Errorf("store: update secret %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: update secret %q: %w", id, ErrNotFound)
	}
	return nil
}

// Delete removes the secret with the given id. It returns [ErrNotFound] if no
// such secret exists.
func (s *SecretStore) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM secrets WHERE id = $1`
	tag, err := s.db.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: delete secret %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete secret %q: %w", id, ErrNotFound)
	}
	return nil
}
