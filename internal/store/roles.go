package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Sentinel errors specific to the RBAC stores. Callers branch with errors.Is.
var (
	// ErrInvalidPermission is returned by the role mutators when a supplied
	// permission string is not in the application catalog ([IsValidPermission]).
	// The wrapped error names the offending value.
	ErrInvalidPermission = errors.New("store: invalid permission")

	// ErrImmutableBuiltin is returned when a mutation would change a built-in
	// role's name or permission set, or delete it. Built-in roles' agent access
	// and assignments are still editable (ADR-0006 §1); only name/permissions and
	// deletion are locked.
	ErrImmutableBuiltin = errors.New("store: builtin role is immutable")

	// ErrLastOwner is returned when removing a role assignment would leave the
	// org with no Owner assignment, violating the guardrail in ADR-0006 §7.
	ErrLastOwner = errors.New("store: cannot remove the last owner assignment")
)

// txBeginner is satisfied by both *pgxpool.Pool (via internal/db.Pool) and
// pgx.Tx, so a store constructed with either can open a transaction for its
// multi-table writes. A pgx.Tx opens a nested (savepoint) transaction.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// RoleStore reads and writes the roles table and its role_permissions /
// role_agents child tables. Writes that span more than one table
// ([RoleStore.Create], [RoleStore.SetPermissions], [RoleStore.SetAgents]) run
// inside a transaction so a role and its permissions/agents move together.
type RoleStore struct {
	db Querier
}

// NewRoleStore returns a RoleStore backed by db.
func NewRoleStore(db Querier) *RoleStore {
	return &RoleStore{db: db}
}

const roleColumns = "id, org_id, name, description, builtin, all_agents, created_at"

func scanRole(row pgx.Row) (Role, error) {
	var r Role
	err := row.Scan(&r.ID, &r.OrgID, &r.Name, &r.Description, &r.Builtin, &r.AllAgents, &r.CreatedAt)
	return r, err
}

// CreateRoleParams carries the fields for [RoleStore.Create]. Description is
// optional (nil stores NULL). Permissions must all be catalog-valid; Agents may
// carry per-grant policy/fan-out overrides. When AllAgents is true the Agents
// set is retained but ignored for authorization (ADR-0006 §3).
type CreateRoleParams struct {
	OrgID       string
	Name        string
	Description *string
	Builtin     bool
	AllAgents   bool
	Permissions []Permission
	Agents      []RoleAgentGrant
}

// RoleAgentGrant is one agent grant supplied to [RoleStore.Create] /
// [RoleStore.SetAgents], with optional per-grant overrides.
type RoleAgentGrant struct {
	AgentID         string
	PolicyVersionID *string
	MaxFanOut       *int
}

// Create inserts a role together with its permission set and agent access and
// returns the hydrated role. A duplicate (org_id, name) yields [ErrConflict]; an
// unknown permission yields [ErrInvalidPermission] (nothing is written). The
// three tables are written in one transaction.
func (s *RoleStore) Create(ctx context.Context, p CreateRoleParams) (Role, error) {
	perms, err := validatePermissions(p.Permissions)
	if err != nil {
		return Role{}, fmt.Errorf("store: create role %q: %w", p.Name, err)
	}
	agents := dedupeGrants(p.Agents)

	tx, err := s.begin(ctx)
	if err != nil {
		return Role{}, fmt.Errorf("store: create role %q: %w", p.Name, err)
	}
	defer tx.Rollback(ctx)

	const q = `INSERT INTO roles (org_id, name, description, builtin, all_agents)
		VALUES ($1, $2, $3, $4, $5) RETURNING ` + roleColumns
	role, err := scanRole(tx.QueryRow(ctx, q, p.OrgID, p.Name, p.Description, p.Builtin, p.AllAgents))
	if err != nil {
		return Role{}, fmt.Errorf("store: create role %q: %w", p.Name, mapError(err))
	}
	if err := insertPermissions(ctx, tx, role.ID, perms); err != nil {
		return Role{}, fmt.Errorf("store: create role %q: %w", p.Name, mapError(err))
	}
	if err := insertAgents(ctx, tx, role.ID, agents); err != nil {
		return Role{}, fmt.Errorf("store: create role %q: %w", p.Name, mapError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return Role{}, fmt.Errorf("store: create role %q: %w", p.Name, mapError(err))
	}

	role.Permissions = perms
	role.Agents = grantsToRoleAgents(role.ID, agents)
	return role, nil
}

// GetByID returns the role with the given id, its permissions and its agent
// access hydrated, or [ErrNotFound].
func (s *RoleStore) GetByID(ctx context.Context, id string) (Role, error) {
	const q = `SELECT ` + roleColumns + ` FROM roles WHERE id = $1`
	role, err := scanRole(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Role{}, fmt.Errorf("store: role by id %q: %w", id, mapError(err))
	}
	if role.Permissions, err = s.permissionsFor(ctx, id); err != nil {
		return Role{}, fmt.Errorf("store: role by id %q: %w", id, err)
	}
	if role.Agents, err = s.agentsFor(ctx, id); err != nil {
		return Role{}, fmt.Errorf("store: role by id %q: %w", id, err)
	}
	return role, nil
}

// ListByOrg returns every role belonging to orgID, ordered by name, with each
// role's permissions and agent access hydrated. Hydration uses two batched
// lookups rather than one query per role.
func (s *RoleStore) ListByOrg(ctx context.Context, orgID string) ([]Role, error) {
	const q = `SELECT ` + roleColumns + ` FROM roles WHERE org_id = $1 ORDER BY name`
	rows, err := s.db.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list roles for org %q: %w", orgID, mapError(err))
	}
	roles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Role, error) {
		return scanRole(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list roles for org %q: %w", orgID, mapError(err))
	}
	if len(roles) == 0 {
		return roles, nil
	}

	ids := make([]string, len(roles))
	for i := range roles {
		ids[i] = roles[i].ID
	}
	permsByRole, err := s.permissionsForMany(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list roles for org %q: %w", orgID, err)
	}
	agentsByRole, err := s.agentsForMany(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list roles for org %q: %w", orgID, err)
	}
	for i := range roles {
		roles[i].Permissions = permsByRole[roles[i].ID]
		roles[i].Agents = agentsByRole[roles[i].ID]
	}
	return roles, nil
}

// UpdateRoleParams carries the editable base fields for [RoleStore.Update]. Name
// and Description replace the stored values; AllAgents flips the all_agents flag.
// A built-in role's Name may not change (ADR-0006 §1); its all_agents and
// description remain editable.
type UpdateRoleParams struct {
	Name        string
	Description *string
	AllAgents   bool
}

// Update replaces a role's base fields (name, description, all_agents). It
// rejects renaming a built-in role with [ErrImmutableBuiltin]; permissions and
// agent access are managed by [RoleStore.SetPermissions] / [RoleStore.SetAgents].
// A duplicate (org_id, name) yields [ErrConflict]; a missing role [ErrNotFound].
func (s *RoleStore) Update(ctx context.Context, id string, p UpdateRoleParams) (Role, error) {
	existing, err := s.GetByID(ctx, id)
	if err != nil {
		return Role{}, fmt.Errorf("store: update role %q: %w", id, err)
	}
	if existing.Builtin && p.Name != existing.Name {
		return Role{}, fmt.Errorf("store: update role %q: rename %q: %w", id, existing.Name, ErrImmutableBuiltin)
	}

	const q = `UPDATE roles SET name = $2, description = $3, all_agents = $4 WHERE id = $1 RETURNING ` + roleColumns
	role, err := scanRole(s.db.QueryRow(ctx, q, id, p.Name, p.Description, p.AllAgents))
	if err != nil {
		return Role{}, fmt.Errorf("store: update role %q: %w", id, mapError(err))
	}
	role.Permissions = existing.Permissions
	role.Agents = existing.Agents
	return role, nil
}

// SetPermissions replaces a role's permission set wholesale. It rejects changes
// to a built-in role with [ErrImmutableBuiltin] and unknown permissions with
// [ErrInvalidPermission]; a missing role yields [ErrNotFound]. The delete and
// re-insert run in one transaction.
func (s *RoleStore) SetPermissions(ctx context.Context, id string, perms []Permission) error {
	valid, err := validatePermissions(perms)
	if err != nil {
		return fmt.Errorf("store: set permissions for role %q: %w", id, err)
	}
	role, err := s.getBase(ctx, id)
	if err != nil {
		return fmt.Errorf("store: set permissions for role %q: %w", id, err)
	}
	if role.Builtin {
		return fmt.Errorf("store: set permissions for role %q: %w", id, ErrImmutableBuiltin)
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("store: set permissions for role %q: %w", id, err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id = $1`, id); err != nil {
		return fmt.Errorf("store: set permissions for role %q: %w", id, mapError(err))
	}
	if err := insertPermissions(ctx, tx, id, valid); err != nil {
		return fmt.Errorf("store: set permissions for role %q: %w", id, mapError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: set permissions for role %q: %w", id, mapError(err))
	}
	return nil
}

// SetAgents replaces a role's explicit agent access wholesale, preserving each
// grant's optional overrides. It is permitted for built-in and custom roles
// alike (a built-in's agent access is editable, ADR-0006 §1). A missing role
// yields [ErrNotFound]. The delete and re-insert run in one transaction.
func (s *RoleStore) SetAgents(ctx context.Context, id string, agents []RoleAgentGrant) error {
	role, err := s.getBase(ctx, id)
	if err != nil {
		return fmt.Errorf("store: set agents for role %q: %w", id, err)
	}
	_ = role // existence check only; both builtin and custom roles are editable here
	grants := dedupeGrants(agents)

	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("store: set agents for role %q: %w", id, err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM role_agents WHERE role_id = $1`, id); err != nil {
		return fmt.Errorf("store: set agents for role %q: %w", id, mapError(err))
	}
	if err := insertAgents(ctx, tx, id, grants); err != nil {
		return fmt.Errorf("store: set agents for role %q: %w", id, mapError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: set agents for role %q: %w", id, mapError(err))
	}
	return nil
}

// Delete removes a custom role. It rejects deleting a built-in role with
// [ErrImmutableBuiltin] (which, since Owner is built-in, also protects the
// Owner role from deletion). The role_permissions / role_agents / member_roles /
// team_roles rows cascade. A missing role yields [ErrNotFound].
func (s *RoleStore) Delete(ctx context.Context, id string) error {
	role, err := s.getBase(ctx, id)
	if err != nil {
		return fmt.Errorf("store: delete role %q: %w", id, err)
	}
	if role.Builtin {
		return fmt.Errorf("store: delete role %q: %w", id, ErrImmutableBuiltin)
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM roles WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete role %q: %w", id, mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: delete role %q: %w", id, ErrNotFound)
	}
	return nil
}

// getBase reads a role's base row (no hydration), translating a missing row to
// [ErrNotFound].
func (s *RoleStore) getBase(ctx context.Context, id string) (Role, error) {
	const q = `SELECT ` + roleColumns + ` FROM roles WHERE id = $1`
	role, err := scanRole(s.db.QueryRow(ctx, q, id))
	if err != nil {
		return Role{}, mapError(err)
	}
	return role, nil
}

func (s *RoleStore) begin(ctx context.Context) (pgx.Tx, error) {
	tb, ok := s.db.(txBeginner)
	if !ok {
		return nil, fmt.Errorf("db does not support transactions")
	}
	return tb.Begin(ctx)
}

// permissionsFor returns the catalog-valid permissions of a single role, ordered
// by permission for a stable result.
func (s *RoleStore) permissionsFor(ctx context.Context, roleID string) ([]Permission, error) {
	const q = `SELECT permission FROM role_permissions WHERE role_id = $1 ORDER BY permission`
	rows, err := s.db.Query(ctx, q, roleID)
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", mapError(err))
	}
	perms, err := pgx.CollectRows(rows, pgx.RowTo[Permission])
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", mapError(err))
	}
	return filterValidPermissions(perms), nil
}

// permissionsForMany returns the catalog-valid permissions for every role id,
// keyed by role id, each slice ordered by permission.
func (s *RoleStore) permissionsForMany(ctx context.Context, roleIDs []string) (map[string][]Permission, error) {
	const q = `SELECT role_id, permission FROM role_permissions WHERE role_id = ANY($1) ORDER BY role_id, permission`
	rows, err := s.db.Query(ctx, q, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("load permissions: %w", mapError(err))
	}
	defer rows.Close()
	out := make(map[string][]Permission, len(roleIDs))
	for rows.Next() {
		var roleID string
		var perm Permission
		if err := rows.Scan(&roleID, &perm); err != nil {
			return nil, fmt.Errorf("load permissions: %w", mapError(err))
		}
		if IsValidPermission(perm) {
			out[roleID] = append(out[roleID], perm)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load permissions: %w", mapError(err))
	}
	return out, nil
}

// agentsFor returns the explicit agent access of a single role, ordered by
// agent id.
func (s *RoleStore) agentsFor(ctx context.Context, roleID string) ([]RoleAgent, error) {
	const q = `SELECT role_id, agent_id, policy_version_id, max_fan_out
		FROM role_agents WHERE role_id = $1 ORDER BY agent_id`
	rows, err := s.db.Query(ctx, q, roleID)
	if err != nil {
		return nil, fmt.Errorf("load agents: %w", mapError(err))
	}
	agents, err := pgx.CollectRows(rows, scanRoleAgentRow)
	if err != nil {
		return nil, fmt.Errorf("load agents: %w", mapError(err))
	}
	return agents, nil
}

// agentsForMany returns the explicit agent access for every role id, keyed by
// role id, each slice ordered by agent id.
func (s *RoleStore) agentsForMany(ctx context.Context, roleIDs []string) (map[string][]RoleAgent, error) {
	const q = `SELECT role_id, agent_id, policy_version_id, max_fan_out
		FROM role_agents WHERE role_id = ANY($1) ORDER BY role_id, agent_id`
	rows, err := s.db.Query(ctx, q, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("load agents: %w", mapError(err))
	}
	agents, err := pgx.CollectRows(rows, scanRoleAgentRow)
	if err != nil {
		return nil, fmt.Errorf("load agents: %w", mapError(err))
	}
	out := make(map[string][]RoleAgent, len(roleIDs))
	for _, a := range agents {
		out[a.RoleID] = append(out[a.RoleID], a)
	}
	return out, nil
}

func scanRoleAgentRow(row pgx.CollectableRow) (RoleAgent, error) {
	var a RoleAgent
	err := row.Scan(&a.RoleID, &a.AgentID, &a.PolicyVersionID, &a.MaxFanOut)
	return a, err
}

// insertPermissions writes a role's permission rows. perms is assumed validated.
func insertPermissions(ctx context.Context, db Querier, roleID string, perms []Permission) error {
	for _, p := range perms {
		if _, err := db.Exec(ctx, `INSERT INTO role_permissions (role_id, permission) VALUES ($1, $2)`, roleID, p); err != nil {
			return err
		}
	}
	return nil
}

// insertAgents writes a role's agent-access rows. grants is assumed deduped.
func insertAgents(ctx context.Context, db Querier, roleID string, grants []RoleAgentGrant) error {
	for _, g := range grants {
		const q = `INSERT INTO role_agents (role_id, agent_id, policy_version_id, max_fan_out) VALUES ($1, $2, $3, $4)`
		if _, err := db.Exec(ctx, q, roleID, g.AgentID, g.PolicyVersionID, g.MaxFanOut); err != nil {
			return err
		}
	}
	return nil
}

// validatePermissions dedupes perms (preserving catalog order) and rejects any
// value outside the catalog with [ErrInvalidPermission].
func validatePermissions(perms []Permission) ([]Permission, error) {
	seen := make(map[Permission]struct{}, len(perms))
	for _, p := range perms {
		if !IsValidPermission(p) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidPermission, p)
		}
		seen[p] = struct{}{}
	}
	out := make([]Permission, 0, len(seen))
	for _, p := range allPermissions {
		if _, ok := seen[p]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// filterValidPermissions drops any non-catalog permission (fail-closed reads).
func filterValidPermissions(perms []Permission) []Permission {
	out := perms[:0]
	for _, p := range perms {
		if IsValidPermission(p) {
			out = append(out, p)
		}
	}
	return out
}

// dedupeGrants removes duplicate agent ids, keeping the last grant for each so
// the caller's most recent override wins.
func dedupeGrants(grants []RoleAgentGrant) []RoleAgentGrant {
	if len(grants) == 0 {
		return nil
	}
	idx := make(map[string]int, len(grants))
	out := make([]RoleAgentGrant, 0, len(grants))
	for _, g := range grants {
		if i, ok := idx[g.AgentID]; ok {
			out[i] = g
			continue
		}
		idx[g.AgentID] = len(out)
		out = append(out, g)
	}
	return out
}

// grantsToRoleAgents projects the create/set inputs into hydrated RoleAgent rows
// for the returned Role.
func grantsToRoleAgents(roleID string, grants []RoleAgentGrant) []RoleAgent {
	if len(grants) == 0 {
		return nil
	}
	out := make([]RoleAgent, len(grants))
	for i, g := range grants {
		out[i] = RoleAgent{RoleID: roleID, AgentID: g.AgentID, PolicyVersionID: g.PolicyVersionID, MaxFanOut: g.MaxFanOut}
	}
	return out
}
