package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// prefixedRoleColumns is roleColumns aliased to the "r" table, for the joins in
// the member_roles / team_roles stores.
const prefixedRoleColumns = "r.id, r.org_id, r.name, r.description, r.builtin, r.all_agents, r.created_at"

// hydrateRoleList runs a single-argument role query whose SELECT list is
// prefixedRoleColumns, collects the roles, and hydrates each with its
// permissions and agent access using two batched lookups. subject is a
// human-readable description of the filter for error context.
func hydrateRoleList(ctx context.Context, db Querier, q, arg, subject string) ([]Role, error) {
	rows, err := db.Query(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list roles for %s: %w", subject, mapError(err))
	}
	roles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Role, error) {
		return scanRole(row)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list roles for %s: %w", subject, mapError(err))
	}
	if len(roles) == 0 {
		return roles, nil
	}

	ids := make([]string, len(roles))
	for i := range roles {
		ids[i] = roles[i].ID
	}
	rs := &RoleStore{db: db}
	permsByRole, err := rs.permissionsForMany(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list roles for %s: %w", subject, err)
	}
	agentsByRole, err := rs.agentsForMany(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: list roles for %s: %w", subject, err)
	}
	for i := range roles {
		roles[i].Permissions = permsByRole[roles[i].ID]
		roles[i].Agents = agentsByRole[roles[i].ID]
	}
	return roles, nil
}
