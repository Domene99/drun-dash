package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MemberStore reads and writes the members table.
type MemberStore struct {
	db Querier
}

// NewMemberStore returns a MemberStore backed by db.
func NewMemberStore(db Querier) *MemberStore {
	return &MemberStore{db: db}
}

const memberColumns = "id, org_id, idp_subject, email, display_name, role, active, created_at, password_hash"

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.OrgID, &m.IDPSubject, &m.Email, &m.DisplayName, &m.Role, &m.Active, &m.CreatedAt, &m.PasswordHash)
	return m, err
}

// CreateMemberParams carries the fields for [MemberStore.Create]. DisplayName
// and PasswordHash are optional (nil stores NULL). An empty Role defaults to
// [RoleMember], matching the column default. Active is optional: nil leaves the
// column default (true); a non-nil value sets it (invite creates inactive
// members, ADR-0008 §6).
type CreateMemberParams struct {
	OrgID        string
	IDPSubject   string
	Email        string
	DisplayName  *string
	Role         MemberRole
	PasswordHash *string
	Active       *bool
}

// Create inserts a member and returns the stored row, letting the database
// assign id and created_at. A duplicate (org_id, idp_subject) or a duplicate
// (org_id, lower(email)) — the case-insensitive per-org email uniqueness added
// by migration 000008 — yields [ErrConflict].
func (s *MemberStore) Create(ctx context.Context, p CreateMemberParams) (Member, error) {
	role := p.Role
	if role == "" {
		role = RoleMember
	}
	// COALESCE lets a nil Active fall back to the column default (true) while a
	// supplied value (e.g. false for a pending invite) takes effect.
	const q = `INSERT INTO members (org_id, idp_subject, email, display_name, role, active, password_hash)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, TRUE), $7) RETURNING ` + memberColumns
	member, err := scanMember(s.db.QueryRow(ctx, q, p.OrgID, p.IDPSubject, p.Email, p.DisplayName, role, p.Active, p.PasswordHash))
	if err != nil {
		return Member{}, fmt.Errorf("store: create member %q: %w", p.IDPSubject, mapError(err))
	}
	return member, nil
}

// GetByID returns the member with the given id, or [ErrNotFound].
func (s *MemberStore) GetByID(ctx context.Context, id string) (Member, error) {
	const q = `SELECT ` + memberColumns + ` FROM members WHERE id = $1`
	member, err := scanMember(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Member{}, fmt.Errorf("store: member by id %q: %w", id, mapError(err))
	}
	return member, nil
}

// GetByIDPSubject returns the member identified by idpSubject within orgID, or
// [ErrNotFound]. The (org_id, idp_subject) pair is unique, so at most one row
// matches.
func (s *MemberStore) GetByIDPSubject(ctx context.Context, orgID, idpSubject string) (Member, error) {
	const q = `SELECT ` + memberColumns + ` FROM members WHERE org_id = $1 AND idp_subject = $2`
	member, err := scanMember(s.db.QueryRow(ctx, q, orgID, idpSubject))
	if err != nil {
		return Member{}, fmt.Errorf("store: member by idp subject %q: %w", idpSubject, mapError(err))
	}
	return member, nil
}

// GetByOrgEmail returns the member identified by email within orgID, matched
// case-insensitively on lower(email), or [ErrNotFound]. It backs password login
// (ADR-0008 §3a/§5): the (org_id, lower(email)) unique index guarantees at most
// one match.
func (s *MemberStore) GetByOrgEmail(ctx context.Context, orgID, email string) (Member, error) {
	const q = `SELECT ` + memberColumns + ` FROM members WHERE org_id = $1 AND lower(email) = lower($2)`
	member, err := scanMember(s.db.QueryRow(ctx, q, orgID, email))
	if err != nil {
		return Member{}, fmt.Errorf("store: member by org email %q: %w", email, mapError(err))
	}
	return member, nil
}

// ListByOrg returns every member of orgID, ordered by email.
func (s *MemberStore) ListByOrg(ctx context.Context, orgID string) ([]Member, error) {
	const q = `SELECT ` + memberColumns + ` FROM members WHERE org_id = $1 ORDER BY email`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list members for org %q: %w", orgID, mapError(err))
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		return scanMember(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list members for org %q: %w", orgID, mapError(err))
	}
	return members, nil
}

// SetActive flips the active flag on the member with the given id and returns
// the updated row. It returns [ErrNotFound] if no such member exists.
func (s *MemberStore) SetActive(ctx context.Context, id string, active bool) (Member, error) {
	const q = `UPDATE members SET active = $2 WHERE id = $1 RETURNING ` + memberColumns
	member, err := scanMember(s.db.QueryRow(ctx, q, id, active))
	if err != nil {
		return Member{}, fmt.Errorf("store: set member %q active=%t: %w", id, active, mapError(err))
	}
	return member, nil
}

// SetPasswordHash sets (or replaces) the bcrypt password hash on the member with
// the given id, enabling password login for that member (ADR-0008 §1/§2). The
// hash is produced by the caller (internal/httpapi); the store only persists it.
// It returns [ErrNotFound] if no such member exists.
func (s *MemberStore) SetPasswordHash(ctx context.Context, id, hash string) error {
	const q = `UPDATE members SET password_hash = $2 WHERE id = $1`
	tag, err := s.db.Exec(ctx, q, id, hash)
	if err != nil {
		return fmt.Errorf("store: set password for member %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: set password for member %q: %w", id, ErrNotFound)
	}
	return nil
}
