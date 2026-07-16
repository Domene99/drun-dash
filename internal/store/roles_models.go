package store

import "time"

// The structs below map to the RBAC tables created by migration 000007: roles
// and the role_permissions / role_agents / member_roles / team_roles joins.
// Conventions match models.go — UUIDs are strings, TIMESTAMPTZ is time.Time,
// and nullable columns are pointers.

// Role is an org-scoped, named object bundling a set of permissions and agent
// access (table roles; unique per (org_id, name)). Built-in roles
// (builtin = true) have an immutable name and permission set; custom roles are
// fully editable. See ADR-0006 §1.
//
// Permissions and Agents are hydrated by the reads that load them
// ([RoleStore.GetByID], [RoleStore.ListByOrg]); mutators that touch only the
// base row leave them nil.
type Role struct {
	ID          string    // id (uuid)
	OrgID       string    // org_id → organizations.id
	Name        string    // name
	Description *string   // description (nullable)
	Builtin     bool      // builtin
	AllAgents   bool      // all_agents — when true, grants every agent in the org
	CreatedAt   time.Time // created_at

	// Permissions is the role's permission set (table role_permissions). Only
	// catalog-valid permissions are surfaced; unknown strings are dropped.
	Permissions []Permission
	// Agents is the role's explicit agent access (table role_agents). It is
	// ignored for authorization when AllAgents is true, but still carried so the
	// admin surface can present it.
	Agents []RoleAgent
}

// RoleAgent is one agent granted by a role, with the optional per-grant tuning
// carried over from entitlements (table role_agents; PK (role_id, agent_id)).
// PolicyVersionID overrides the agent version's pinned policy for this grant;
// MaxFanOut overrides its fan-out ceiling. Both are optional (nil → the agent
// version's own pin stands). See ADR-0006 §3.
type RoleAgent struct {
	RoleID          string  // role_id → roles.id
	AgentID         string  // agent_id → agents.id
	PolicyVersionID *string // policy_version_id → policy_versions.id (nullable)
	MaxFanOut       *int    // max_fan_out (nullable, >= 0)
}

// MemberRoleAssignment assigns a [Role] directly to a [Member] (join table
// member_roles, PK (member_id, role_id)). See ADR-0006 §4.
type MemberRoleAssignment struct {
	MemberID string // member_id → members.id
	RoleID   string // role_id → roles.id
}

// TeamRoleAssignment assigns a [Role] to a [Team]; every member of the team
// inherits it (join table team_roles, PK (team_id, role_id)). See ADR-0006 §4.
type TeamRoleAssignment struct {
	TeamID string // team_id → teams.id
	RoleID string // role_id → roles.id
}
