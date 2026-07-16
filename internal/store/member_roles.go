package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MemberRoleStore reads and writes the member_roles join table, which assigns
// roles directly to members (ADR-0006 §4).
type MemberRoleStore struct {
	db Querier
}

// NewMemberRoleStore returns a MemberRoleStore backed by db.
func NewMemberRoleStore(db Querier) *MemberRoleStore {
	return &MemberRoleStore{db: db}
}

// AssignRole assigns roleID to memberID and returns the stored row. Assigning a
// role the member already holds violates the (member_id, role_id) primary key
// and yields [ErrConflict].
func (s *MemberRoleStore) AssignRole(ctx context.Context, memberID, roleID string) (MemberRoleAssignment, error) {
	const q = `INSERT INTO member_roles (member_id, role_id) VALUES ($1, $2)
		RETURNING member_id, role_id`
	var mr MemberRoleAssignment
	if err := s.db.QueryRow(ctx, q, memberID, roleID).Scan(&mr.MemberID, &mr.RoleID); err != nil {
		return MemberRoleAssignment{}, fmt.Errorf("store: assign role %q to member %q: %w", roleID, memberID, mapError(err))
	}
	return mr, nil
}

// RemoveRole unassigns roleID from memberID. It returns [ErrNotFound] if the
// member did not hold the role, and [ErrLastOwner] if the role is the org's
// built-in Owner and removing it would leave the org with no Owner assignment
// (ADR-0006 §7).
func (s *MemberRoleStore) RemoveRole(ctx context.Context, memberID, roleID string) error {
	if err := guardLastOwner(ctx, s.db, roleID); err != nil {
		return fmt.Errorf("store: remove role %q from member %q: %w", roleID, memberID, err)
	}
	const q = `DELETE FROM member_roles WHERE member_id = $1 AND role_id = $2`
	tag, err := s.db.Exec(ctx, q, memberID, roleID)
	if err != nil {
		return fmt.Errorf("store: remove role %q from member %q: %w", roleID, memberID, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: remove role %q from member %q: %w", roleID, memberID, ErrNotFound)
	}
	return nil
}

// ListRolesForMember returns the roles assigned directly to memberID (not the
// roles it inherits via teams), ordered by name, with permissions and agent
// access hydrated. An empty result is not an error.
func (s *MemberRoleStore) ListRolesForMember(ctx context.Context, memberID string) ([]Role, error) {
	const q = `SELECT ` + prefixedRoleColumns + `
		FROM roles r
		JOIN member_roles mr ON mr.role_id = r.id
		WHERE mr.member_id = $1
		ORDER BY r.name`
	return hydrateRoleList(ctx, s.db, q, memberID, fmt.Sprintf("member %q", memberID))
}

// ListMembersForRole returns the members assigned roleID directly, ordered by
// email. An empty result is not an error.
func (s *MemberRoleStore) ListMembersForRole(ctx context.Context, roleID string) ([]Member, error) {
	const q = `SELECT m.id, m.org_id, m.idp_subject, m.email, m.display_name, m.role, m.active, m.created_at, m.password_hash
		FROM members m
		JOIN member_roles mr ON mr.member_id = m.id
		WHERE mr.role_id = $1
		ORDER BY m.email`
	rows, err := s.db.Query(ctx, q, roleID)
	if err != nil {
		return nil, fmt.Errorf("store: list members for role %q: %w", roleID, mapError(err))
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		return scanMember(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list members for role %q: %w", roleID, mapError(err))
	}
	return members, nil
}

// guardLastOwner returns [ErrLastOwner] when roleID is a built-in Owner role and
// it holds the org's only remaining Owner assignment (counting both member and
// team assignments). A non-Owner or missing role is not guarded here; the caller
// surfaces [ErrNotFound] from the delete itself.
func guardLastOwner(ctx context.Context, db Querier, roleID string) error {
	const roleQ = `SELECT org_id, builtin, name FROM roles WHERE id = $1`
	var orgID, name string
	var builtin bool
	if err := db.QueryRow(ctx, roleQ, roleID).Scan(&orgID, &builtin, &name); err != nil {
		// A missing role is not a last-owner problem; let the delete report it.
		if err == pgx.ErrNoRows {
			return nil
		}
		return mapError(err)
	}
	if !builtin || name != BuiltinOwner {
		return nil
	}
	if count, err := ownerAssignmentCount(ctx, db, orgID); err != nil {
		return err
	} else if count <= 1 {
		return ErrLastOwner
	}
	return nil
}

// ownerAssignmentCount counts the org's Owner assignments across member_roles
// and team_roles.
func ownerAssignmentCount(ctx context.Context, db Querier, orgID string) (int, error) {
	const q = `SELECT
		(SELECT count(*) FROM member_roles mr JOIN roles r ON r.id = mr.role_id
			WHERE r.org_id = $1 AND r.builtin AND r.name = $2)
		+ (SELECT count(*) FROM team_roles tr JOIN roles r ON r.id = tr.role_id
			WHERE r.org_id = $1 AND r.builtin AND r.name = $2)`
	var count int
	if err := db.QueryRow(ctx, q, orgID, BuiltinOwner).Scan(&count); err != nil {
		return 0, mapError(err)
	}
	return count, nil
}
