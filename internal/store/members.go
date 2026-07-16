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

const memberColumns = "id, org_id, idp_subject, email, display_name, role, active, created_at"

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.ID, &m.OrgID, &m.IDPSubject, &m.Email, &m.DisplayName, &m.Role, &m.Active, &m.CreatedAt)
	return m, err
}

// CreateMemberParams carries the fields for [MemberStore.Create]. DisplayName
// is optional (nil stores NULL). An empty Role defaults to [RoleMember],
// matching the column default.
type CreateMemberParams struct {
	OrgID       string
	IDPSubject  string
	Email       string
	DisplayName *string
	Role        MemberRole
}

// Create inserts a member and returns the stored row, letting the database
// assign id, active (true), and created_at. A duplicate (org_id, idp_subject)
// yields [ErrConflict].
func (s *MemberStore) Create(ctx context.Context, p CreateMemberParams) (Member, error) {
	role := p.Role
	if role == "" {
		role = RoleMember
	}
	const q = `INSERT INTO members (org_id, idp_subject, email, display_name, role)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + memberColumns
	member, err := scanMember(s.db.QueryRow(ctx, q, p.OrgID, p.IDPSubject, p.Email, p.DisplayName, role))
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
