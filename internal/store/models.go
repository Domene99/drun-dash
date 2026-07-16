package store

import "time"

// The structs below map one-to-one to the tenancy and identity tables created
// by migrations 000001, 000002, and 000008. UUID columns are represented as
// strings (pgx scans and encodes them transparently) and TIMESTAMPTZ columns as
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

// Member is a human principal (table members; unique per (org_id, idp_subject)).
// A member authenticates via OIDC (idp_subject maps an upstream IdP subject) or
// email+password (PasswordHash) or both (ADR-0008 §1). Password-only members
// carry a synthetic "local:<opaque>" idp_subject so the NOT-NULL/unique index
// stays intact; email is additionally unique per org, case-insensitively.
type Member struct {
	ID           string     // id (uuid)
	OrgID        string     // org_id → organizations.id
	IDPSubject   string     // idp_subject ("local:<opaque>" for password-only members)
	Email        string     // email (unique per org on lower(email))
	DisplayName  *string    // display_name (nullable)
	Role         MemberRole // role (member_role enum)
	Active       bool       // active
	CreatedAt    time.Time  // created_at
	PasswordHash *string    // password_hash (nullable; NULL for OIDC-only members)
}

// TeamMembership links a [Member] to a [Team] (join table team_memberships,
// primary key (team_id, member_id)).
type TeamMembership struct {
	TeamID   string    // team_id → teams.id
	MemberID string    // member_id → members.id
	AddedAt  time.Time // added_at
}

// Invitation is a pending member invitation (table invitations, ADR-0008 §6/§8).
// The invited [Member] row is created inactive up front; this row carries the
// single-use, TTL'd token — persisted only as its SHA-256 hash, never the raw
// value — and the RoleIDs to assign on acceptance. ConsumedAt is nil until the
// invite is accepted, after which the token is spent (single-use).
type Invitation struct {
	ID         string     // id (uuid)
	OrgID      string     // org_id → organizations.id
	MemberID   string     // member_id → members.id (the pending member)
	TokenHash  string     // token_hash (SHA-256 hex of the raw token)
	RoleIDs    []string   // role_ids (uuid[]) assigned to the member on acceptance
	ExpiresAt  time.Time  // expires_at (TTL)
	ConsumedAt *time.Time // consumed_at (nullable; set once accepted)
	CreatedAt  time.Time  // created_at
}
