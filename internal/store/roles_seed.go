package store

import (
	"context"
	"fmt"
)

// SeedBuiltins inserts the four built-in roles — Owner, Admin, Auditor, Member —
// and their locked permission sets ([BuiltinRoles]) for orgID, mirroring the
// per-org seed migration 000007 performs. It is the runtime counterpart the
// migration cannot provide: 000007 seeds only orgs that exist at migration time,
// so an org created at runtime (org self-signup, ADR-0008 §4.2) must seed its
// own roles. Keeping the Go [BuiltinRoles] catalog the single source both this
// and the migration follow is the ADR-0006 lock-step the ADR names.
//
// It runs against the store's [Querier] without opening its own transaction, so
// a caller creating an org can seed roles in the same transaction (pass a
// pgx.Tx). The seeded roles are returned in [BuiltinRoles] order with their
// permission sets hydrated, so the caller can pick out (e.g.) the Owner role to
// assign to the founding member. A duplicate (org_id, name) — seeding an org
// twice — yields [ErrConflict].
func (s *RoleStore) SeedBuiltins(ctx context.Context, orgID string) ([]Role, error) {
	out := make([]Role, 0, len(BuiltinRoles))
	for _, br := range BuiltinRoles {
		const q = `INSERT INTO roles (org_id, name, description, builtin, all_agents)
			VALUES ($1, $2, $3, TRUE, $4) RETURNING ` + roleColumns
		desc := br.Description
		role, err := scanRole(s.db.QueryRow(ctx, q, orgID, br.Name, &desc, br.AllAgents))
		if err != nil {
			return nil, fmt.Errorf("store: seed builtin role %q: %w", br.Name, mapError(err))
		}
		if err := insertPermissions(ctx, s.db, role.ID, br.Permissions); err != nil {
			return nil, fmt.Errorf("store: seed builtin role %q: %w", br.Name, mapError(err))
		}
		role.Permissions = append([]Permission(nil), br.Permissions...)
		out = append(out, role)
	}
	return out, nil
}

// BuiltinRoleByName returns the seeded role with the given built-in name from a
// [RoleStore.SeedBuiltins] result, or a zero Role and false if absent. It saves
// callers a manual loop when they need one specific built-in (typically Owner).
func BuiltinRoleByName(roles []Role, name string) (Role, bool) {
	for _, r := range roles {
		if r.Name == name {
			return r, true
		}
	}
	return Role{}, false
}
