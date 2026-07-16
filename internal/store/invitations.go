package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// InvitationStore reads and writes the invitations table (ADR-0008 §6/§8). It
// persists only the SHA-256 hash of each invite token, never the raw value; the
// raw token lives in the invite link the caller hands off and is presented back
// at acceptance, where it is re-hashed and matched here.
type InvitationStore struct {
	db Querier
}

// NewInvitationStore returns an InvitationStore backed by db.
func NewInvitationStore(db Querier) *InvitationStore {
	return &InvitationStore{db: db}
}

const invitationColumns = "id, org_id, member_id, token_hash, role_ids, expires_at, consumed_at, created_at"

func scanInvitation(row pgx.Row) (Invitation, error) {
	var inv Invitation
	err := row.Scan(&inv.ID, &inv.OrgID, &inv.MemberID, &inv.TokenHash, &inv.RoleIDs, &inv.ExpiresAt, &inv.ConsumedAt, &inv.CreatedAt)
	return inv, err
}

// CreateInvitationParams carries the fields for [InvitationStore.Create].
// TokenHash is the SHA-256 hex of the raw token (computed by the caller);
// RoleIDs is the set of roles to assign to the member on acceptance; ExpiresAt
// is the TTL boundary.
type CreateInvitationParams struct {
	OrgID     string
	MemberID  string
	TokenHash string
	RoleIDs   []string
	ExpiresAt time.Time
}

// Create inserts an invitation and returns the stored row, letting the database
// assign id and created_at (and leaving consumed_at NULL). A duplicate
// token_hash yields [ErrConflict].
func (s *InvitationStore) Create(ctx context.Context, p CreateInvitationParams) (Invitation, error) {
	roleIDs := p.RoleIDs
	if roleIDs == nil {
		roleIDs = []string{}
	}
	const q = `INSERT INTO invitations (org_id, member_id, token_hash, role_ids, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + invitationColumns
	inv, err := scanInvitation(s.db.QueryRow(ctx, q, p.OrgID, p.MemberID, p.TokenHash, roleIDs, p.ExpiresAt))
	if err != nil {
		return Invitation{}, fmt.Errorf("store: create invitation for member %q: %w", p.MemberID, mapError(err))
	}
	return inv, nil
}

// GetByTokenHash returns the invitation whose token_hash matches, or
// [ErrNotFound]. The token_hash index is unique, so at most one row matches. It
// does not filter on expiry or consumption — the caller (or
// [AccountStore.AcceptInvite]) decides how to treat an expired or
// already-consumed invite.
func (s *InvitationStore) GetByTokenHash(ctx context.Context, tokenHash string) (Invitation, error) {
	const q = `SELECT ` + invitationColumns + ` FROM invitations WHERE token_hash = $1`
	inv, err := scanInvitation(s.db.QueryRow(ctx, q, tokenHash))
	if err != nil {
		return Invitation{}, fmt.Errorf("store: invitation by token hash: %w", mapError(err))
	}
	return inv, nil
}

// Consume marks the invitation with the given id as accepted by setting
// consumed_at, enforcing single use: the update only fires when consumed_at is
// still NULL, so a second acceptance affects no rows and returns [ErrNotFound]
// (indistinguishable from an unknown id — an already-spent token reveals
// nothing). A genuine race between two acceptances therefore lets exactly one
// win.
func (s *InvitationStore) Consume(ctx context.Context, id string) error {
	const q = `UPDATE invitations SET consumed_at = now() WHERE id = $1 AND consumed_at IS NULL`
	tag, err := s.db.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("store: consume invitation %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: consume invitation %q: %w", id, ErrNotFound)
	}
	return nil
}

// ListPendingByOrg returns the org's invitations that are neither consumed nor
// expired, most-recent first. An empty result is not an error. It backs the
// admin console's "pending invites" surface (a later batch).
func (s *InvitationStore) ListPendingByOrg(ctx context.Context, orgID string) ([]Invitation, error) {
	const q = `SELECT ` + invitationColumns + `
		FROM invitations
		WHERE org_id = $1 AND consumed_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC, id`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list pending invitations for org %q: %w", orgID, mapError(err))
	}
	invs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invitation, error) {
		return scanInvitation(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list pending invitations for org %q: %w", orgID, mapError(err))
	}
	return invs, nil
}
