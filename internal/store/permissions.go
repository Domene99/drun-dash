package store

// Permission is a coarse-grained capability verb from the fixed, application
// level catalog defined by ADR-0006 §2 — one verb per governable capability,
// not one per endpoint. Permissions are stored as TEXT in the role_permissions
// table and validated here rather than by a PostgreSQL ENUM, so the catalog can
// grow (see endpoints.manage / budgets.manage, reserved for future surfaces)
// with a code change plus a seed update instead of an ALTER TYPE migration.
//
// Unknown permission strings are rejected on write ([IsValidPermission] guards
// the RoleStore mutators) and ignored on read (fail-closed), matching the ADR.
type Permission string

// The permission catalog (ADR-0006 §2). Keep AllPermissions in sync with this
// list; the migration's built-in seed and the tests reference these values.
const (
	// PermAgentsManage governs create/update/delete of agents, publishing agent
	// versions, and managing an agent's role access.
	PermAgentsManage Permission = "agents.manage"
	// PermPoliciesManage governs creating policies and publishing policy versions.
	PermPoliciesManage Permission = "policies.manage"
	// PermEndpointsManage governs managing model endpoints (future surface).
	PermEndpointsManage Permission = "endpoints.manage"
	// PermBudgetsManage governs managing budgets and budget rules (future surface).
	PermBudgetsManage Permission = "budgets.manage"
	// PermMembersManage governs managing members and assigning roles to members
	// and teams.
	PermMembersManage Permission = "members.manage"
	// PermRolesManage governs creating/updating/deleting roles and defining their
	// permissions and agent access.
	PermRolesManage Permission = "roles.manage"
	// PermApprovalsDecide governs approving and denying escalation requests.
	PermApprovalsDecide Permission = "approvals.decide"
	// PermAuditView governs viewing the org-wide audit log, spend analytics, and
	// all sessions/trajectories.
	PermAuditView Permission = "audit.view"
	// PermAgentsRun governs launching sessions — for the agents the granting role
	// also grants (agent access, ADR-0006 §3). Necessary but not sufficient: it
	// is evaluated together with the role's agent access by the resolver.
	PermAgentsRun Permission = "agents.run"
)

// allPermissions is the ordered catalog. The order matches ADR-0006 §2 and is
// the order AllPermissions returns.
var allPermissions = []Permission{
	PermAgentsManage,
	PermPoliciesManage,
	PermEndpointsManage,
	PermBudgetsManage,
	PermMembersManage,
	PermRolesManage,
	PermApprovalsDecide,
	PermAuditView,
	PermAgentsRun,
}

// permissionSet is a lookup built once from allPermissions.
var permissionSet = func() map[Permission]struct{} {
	m := make(map[Permission]struct{}, len(allPermissions))
	for _, p := range allPermissions {
		m[p] = struct{}{}
	}
	return m
}()

// AllPermissions returns a copy of the permission catalog in catalog order. The
// returned slice is safe for the caller to modify.
func AllPermissions() []Permission {
	return append([]Permission(nil), allPermissions...)
}

// IsValidPermission reports whether p is a member of the application catalog.
// Writes are rejected when it returns false; reads drop unknown strings.
func IsValidPermission(p Permission) bool {
	_, ok := permissionSet[p]
	return ok
}

// Built-in role names (ADR-0006 §1). Every org is seeded with exactly these
// four built-in roles; their name and permission set are immutable.
const (
	BuiltinOwner   = "Owner"
	BuiltinAdmin   = "Admin"
	BuiltinAuditor = "Auditor"
	BuiltinMember  = "Member"
)

// BuiltinRole describes the locked shape of a seeded built-in role (ADR-0006
// §1). The migration seeds these per org; the store references them so the Go
// definition and the SQL seed stay aligned.
type BuiltinRole struct {
	Name        string
	Description string
	// AllAgents mirrors roles.all_agents: Owner grants every agent, the others
	// grant none by default (their agent access is still editable).
	AllAgents   bool
	Permissions []Permission
}

// BuiltinRoles is the ordered set of built-in roles every org receives, with the
// permission sets locked by ADR-0006 §1:
//
//   - Owner   — every permission, all_agents = true.
//   - Admin   — all *.manage + approvals.decide + audit.view (not agents.run).
//   - Auditor — audit.view.
//   - Member  — agents.run (agent access assigned per member/team).
var BuiltinRoles = []BuiltinRole{
	{
		Name:        BuiltinOwner,
		Description: "Super-admin: every permission and access to all agents.",
		AllAgents:   true,
		Permissions: AllPermissions(),
	},
	{
		Name:        BuiltinAdmin,
		Description: "Manages the catalog, members, roles, approvals, and audit; does not run agents.",
		AllAgents:   false,
		Permissions: []Permission{
			PermAgentsManage,
			PermPoliciesManage,
			PermEndpointsManage,
			PermBudgetsManage,
			PermMembersManage,
			PermRolesManage,
			PermApprovalsDecide,
			PermAuditView,
		},
	},
	{
		Name:        BuiltinAuditor,
		Description: "Read-only access to the org-wide audit log, spend, and activity.",
		AllAgents:   false,
		Permissions: []Permission{PermAuditView},
	},
	{
		Name:        BuiltinMember,
		Description: "May run the agents granted to them; no management permissions.",
		AllAgents:   false,
		Permissions: []Permission{PermAgentsRun},
	},
}
