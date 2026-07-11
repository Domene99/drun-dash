package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CredentialGrantStore reads and writes the credential_grants table.
type CredentialGrantStore struct {
	db Querier
}

// NewCredentialGrantStore returns a CredentialGrantStore backed by db.
func NewCredentialGrantStore(db Querier) *CredentialGrantStore {
	return &CredentialGrantStore{db: db}
}

const credentialGrantColumns = "id, session_id, proxy_token_hash, endpoint_id, scopes, issued_at, expires_at, revoked_at"

func scanCredentialGrant(row pgx.Row) (CredentialGrant, error) {
	var g CredentialGrant
	err := row.Scan(&g.ID, &g.SessionID, &g.ProxyTokenHash, &g.EndpointID, &g.Scopes,
		&g.IssuedAt, &g.ExpiresAt, &g.RevokedAt)
	return g, err
}

// CreateCredentialGrantParams carries the fields for
// [CredentialGrantStore.Create]. ProxyTokenHash is the hash of the proxy token;
// the raw token is never stored. A nil Scopes stores the column default ('[]');
// a nil EndpointID stores NULL. ExpiresAt is required.
type CreateCredentialGrantParams struct {
	SessionID      string
	ProxyTokenHash string
	EndpointID     *string
	Scopes         json.RawMessage
	ExpiresAt      time.Time
}

// Create inserts a credential grant and returns the stored row, letting the
// database assign id and issued_at. A duplicate proxy_token_hash yields
// [ErrConflict]; a missing session (or endpoint) is rejected by the foreign
// key.
func (s *CredentialGrantStore) Create(ctx context.Context, p CreateCredentialGrantParams) (CredentialGrant, error) {
	const q = `INSERT INTO credential_grants (session_id, proxy_token_hash, endpoint_id, scopes, expires_at)
		VALUES ($1, $2, $3, COALESCE($4, '[]'::jsonb), $5) RETURNING ` + credentialGrantColumns
	grant, err := scanCredentialGrant(s.db.QueryRow(ctx, q,
		p.SessionID, p.ProxyTokenHash, p.EndpointID, p.Scopes, p.ExpiresAt))
	if err != nil {
		return CredentialGrant{}, fmt.Errorf("store: create credential grant for session %q: %w", p.SessionID, mapError(err))
	}
	return grant, nil
}

// GetByTokenHash returns the grant whose proxy_token_hash equals hash, or
// [ErrNotFound]. This is the proxy's per-call token lookup; callers must still
// check ExpiresAt and RevokedAt before honouring the grant.
func (s *CredentialGrantStore) GetByTokenHash(ctx context.Context, hash string) (CredentialGrant, error) {
	const q = `SELECT ` + credentialGrantColumns + ` FROM credential_grants WHERE proxy_token_hash = $1`
	grant, err := scanCredentialGrant(s.db.QueryRow(ctx, q, hash))
	if err != nil {
		return CredentialGrant{}, fmt.Errorf("store: credential grant by token hash: %w", mapError(err))
	}
	return grant, nil
}

// ListBySession returns every credential grant minted for sessionID, ordered by
// issued_at then id for a stable order.
func (s *CredentialGrantStore) ListBySession(ctx context.Context, sessionID string) ([]CredentialGrant, error) {
	const q = `SELECT ` + credentialGrantColumns + ` FROM credential_grants WHERE session_id = $1 ORDER BY issued_at, id`
	rows, err := s.db.Query(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list credential grants for session %q: %w", sessionID, mapError(err))
	}
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CredentialGrant, error) {
		return scanCredentialGrant(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list credential grants for session %q: %w", sessionID, mapError(err))
	}
	return grants, nil
}

// Revoke stamps revoked_at with now() on the grant with the given id and
// returns the updated row. It returns [ErrNotFound] if no such grant exists.
// Revoking an already-revoked grant refreshes revoked_at.
func (s *CredentialGrantStore) Revoke(ctx context.Context, id string) (CredentialGrant, error) {
	const q = `UPDATE credential_grants SET revoked_at = now() WHERE id = $1 RETURNING ` + credentialGrantColumns
	grant, err := scanCredentialGrant(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return CredentialGrant{}, fmt.Errorf("store: revoke credential grant %q: %w", id, mapError(err))
	}
	return grant, nil
}
