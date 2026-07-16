package store

import "time"

// The structs below map one-to-one to the tenancy and identity tables created
// by migrations 000001 and 000002. UUID columns are represented as strings
// (pgx scans and encodes them transparently) and TIMESTAMPTZ columns as
// time.Time; nullable columns use pointer fields.

// MemberRole is the coarse authorization role carried on a [Member], mirroring
// the Postgres member_role enum ('admin' | 'member' | 'auditor').
//
// Deprecated for authorization by ADR-0006: the source of truth for access
// becomes the RBAC [Role] tables (member_roles / team_roles). The column and
// this type are retained for one release as identity metadata and to keep the
// backfill reversible.
type MemberRole string

// The member_role enum values. These are the only roles the database accepts.
const (
	RoleAdmin   MemberRole = "admin"
	RoleMember  MemberRole = "member"
	RoleAuditor MemberRole = "auditor"
)

// Valid reports whether r is one of the defined member_role values.
func (r MemberRole) Valid() bool {
	switch r {
	case RoleAdmin, RoleMember, RoleAuditor:
		return true
	default:
		return false
	}
}

// Organization is the top-level tenant boundary (table organizations).
type Organization struct {
	ID        string    // id (uuid)
	Slug      string    // slug (unique)
	Name      string    // name
	CreatedAt time.Time // created_at
}

// Environment scopes resources within an organization, e.g. prod vs. staging
// (table environments; unique per (org_id, name)).
type Environment struct {
	ID        string    // id (uuid)
	OrgID     string    // org_id → organizations.id
	Name      string    // name
	CreatedAt time.Time // created_at
}

// Team groups members for entitlement and budget scoping (table teams; unique
// per (org_id, name)).
type Team struct {
	ID        string    // id (uuid)
	OrgID     string    // org_id → organizations.id
	Name      string    // name
	CreatedAt time.Time // created_at
}

// Member is a human principal mapped from an upstream IdP subject (table
// members; unique per (org_id, idp_subject)).
type Member struct {
	ID          string     // id (uuid)
	OrgID       string     // org_id → organizations.id
	IDPSubject  string     // idp_subject
	Email       string     // email
	DisplayName *string    // display_name (nullable)
	Role        MemberRole // role (member_role enum)
	Active      bool       // active
	CreatedAt   time.Time  // created_at
}

// TeamMembership links a [Member] to a [Team] (join table team_memberships,
// primary key (team_id, member_id)).
type TeamMembership struct {
	TeamID   string    // team_id → teams.id
	MemberID string    // member_id → members.id
	AddedAt  time.Time // added_at
}
