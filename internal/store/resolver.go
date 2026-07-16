package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AccessResolver answers the effective-access questions the enforcement layer
// asks per request (ADR-0006 §4/§6, ADR-0010). It resolves a member's grants
// from the database on every call — never from a token claim — so revocation is
// prompt and fail-closed. Effective access is the union across every role
// reachable directly (member_roles) or via a team (team_roles ∘
// team_memberships).
type AccessResolver struct {
	db Querier
}

// NewAccessResolver returns an AccessResolver backed by db.
func NewAccessResolver(db Querier) *AccessResolver {
	return &AccessResolver{db: db}
}

// reachableRolesCTE is the set of role ids reachable by member $1, directly or
// through a team. It is embedded as a subquery (its single parameter is $1).
const reachableRolesCTE = `
	SELECT role_id FROM member_roles WHERE member_id = $1
	UNION
	SELECT tr.role_id FROM team_roles tr
	JOIN team_memberships tm ON tm.team_id = tr.team_id
	WHERE tm.member_id = $1`

// HasPermission reports whether memberID's effective permission set includes
// perm. It is the indexed EXISTS join the RequirePermission middleware uses
// (ADR-0006 §6). An unknown permission always returns false (fail-closed).
func (r *AccessResolver) HasPermission(ctx context.Context, memberID string, perm Permission) (bool, error) {
	if !IsValidPermission(perm) {
		return false, nil
	}
	const q = `SELECT EXISTS (
		SELECT 1 FROM role_permissions rp
		WHERE rp.permission = $2 AND rp.role_id IN (` + reachableRolesCTE + `)
	)`
	var ok bool
	if err := r.db.QueryRow(ctx, q, memberID, perm).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: has permission %q for member %q: %w", perm, memberID, mapError(err))
	}
	return ok, nil
}

// EffectivePermissions returns the union of every reachable role's permission
// set for memberID, ordered by permission, with unknown strings dropped. An
// empty result is not an error.
func (r *AccessResolver) EffectivePermissions(ctx context.Context, memberID string) ([]Permission, error) {
	const q = `SELECT DISTINCT rp.permission
		FROM role_permissions rp
		WHERE rp.role_id IN (` + reachableRolesCTE + `)
		ORDER BY rp.permission`
	rows, err := r.db.Query(ctx, q, memberID)
	if err != nil {
		return nil, fmt.Errorf("store: effective permissions for member %q: %w", memberID, mapError(err))
	}
	perms, err := pgx.CollectRows(rows, pgx.RowTo[Permission])
	if err != nil {
		return nil, fmt.Errorf("store: effective permissions for member %q: %w", memberID, mapError(err))
	}
	return filterValidPermissions(perms), nil
}

// RunGrant is the resolved outcome of an authorized launch (ADR-0010). Under the
// policy-centric model the agent version's own manifest is the sole authority
// for the launch boundary and fan-out ceiling, so the grant no longer carries a
// policy-version or fan-out override. AllAgents records that the member reached
// the agent through an "all agents" super-admin role; it is retained for callers
// that report the reason access was granted.
type RunGrant struct {
	AllAgents bool
}

// AuthorizeRun decides whether memberID may launch agentID (ADR-0010). The
// predicate, in order:
//
//  1. The member must hold agents.run in their effective permission set; else
//     denied.
//  2. If any reachable role is all_agents, the member is a super-admin and may
//     run every agent → allowed (RunGrant{AllAgents:true}).
//  3. Otherwise resolve the agent's governing policy (its current version's
//     policy_version_id → policy_versions.policy_id). A null governing policy
//     (no current version, or a version with no policy pin) is general access →
//     allowed.
//  4. Else allowed iff the governing policy permits one of the member's
//     reachable roles (a policy_roles row).
//
// The returned RunGrant carries no launch tuning — launch.go derives the
// boundary from the agent version manifest. A missing agent (no such row)
// resolves to denied; the launch handler has already loaded and org-scoped the
// agent, so this is a defensive fail-closed.
func (r *AccessResolver) AuthorizeRun(ctx context.Context, memberID, agentID string) (RunGrant, bool, error) {
	hasRun, err := r.HasPermission(ctx, memberID, PermAgentsRun)
	if err != nil {
		return RunGrant{}, false, fmt.Errorf("store: authorize run of agent %q by member %q: %w", agentID, memberID, err)
	}
	if !hasRun {
		return RunGrant{}, false, nil
	}

	allAgents, err := r.hasAllAgents(ctx, memberID)
	if err != nil {
		return RunGrant{}, false, fmt.Errorf("store: authorize run of agent %q by member %q: %w", agentID, memberID, err)
	}
	if allAgents {
		return RunGrant{AllAgents: true}, true, nil
	}

	allowed, err := r.policyPermitsMember(ctx, memberID, agentID)
	if err != nil {
		return RunGrant{}, false, fmt.Errorf("store: authorize run of agent %q by member %q: %w", agentID, memberID, err)
	}
	return RunGrant{}, allowed, nil
}

// policyPermitsMember resolves agentID's governing policy and reports whether it
// grants access to memberID: true when the agent has no governing policy
// (general access) or when a policy_roles row links the policy to one of the
// member's reachable roles. A missing agent resolves to false (fail-closed).
func (r *AccessResolver) policyPermitsMember(ctx context.Context, memberID, agentID string) (bool, error) {
	// pv.policy_id is NULL when the agent has no current version or its current
	// version pins no policy → general access. Otherwise the EXISTS join tests
	// the policy's permitted-role set against the member's reachable roles.
	const q = `SELECT
		pv.policy_id IS NULL
		OR EXISTS (
			SELECT 1 FROM policy_roles pr
			WHERE pr.policy_id = pv.policy_id
			  AND pr.role_id IN (` + reachableRolesCTE + `)
		)
	FROM agents a
	LEFT JOIN agent_versions av ON av.agent_id = a.id AND av.version = a.current_version
	LEFT JOIN policy_versions pv ON pv.id = av.policy_version_id
	WHERE a.id = $2`
	var ok bool
	if err := r.db.QueryRow(ctx, q, memberID, agentID).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, mapError(err)
	}
	return ok, nil
}

// ListAccessibleAgentIDs returns the ids of the agents in orgID that memberID
// may see and run under the policy-centric model (ADR-0010), ordered by
// (created_at, id) for a stable list. It backs GET /me/agents. The set is:
//
//   - empty if the member lacks agents.run;
//   - every agent in the org if any reachable role is all_agents;
//   - otherwise every agent whose governing policy is NULL (general access) or
//     whose policy permits one of the member's reachable roles.
func (r *AccessResolver) ListAccessibleAgentIDs(ctx context.Context, memberID, orgID string) ([]string, error) {
	hasRun, err := r.HasPermission(ctx, memberID, PermAgentsRun)
	if err != nil {
		return nil, fmt.Errorf("store: list accessible agents for member %q: %w", memberID, err)
	}
	if !hasRun {
		return nil, nil
	}

	allAgents, err := r.hasAllAgents(ctx, memberID)
	if err != nil {
		return nil, fmt.Errorf("store: list accessible agents for member %q: %w", memberID, err)
	}
	if allAgents {
		return r.agentIDs(ctx, `SELECT a.id FROM agents a WHERE a.org_id = $2 ORDER BY a.created_at, a.id`, memberID, orgID)
	}

	// A governing policy is NULL (general access) or must permit one of the
	// member's reachable roles. $1 feeds the reachable-roles CTE, $2 the org.
	const q = `SELECT a.id
		FROM agents a
		LEFT JOIN agent_versions av ON av.agent_id = a.id AND av.version = a.current_version
		LEFT JOIN policy_versions pv ON pv.id = av.policy_version_id
		WHERE a.org_id = $2
		  AND (
			pv.policy_id IS NULL
			OR pv.policy_id IN (
				SELECT pr.policy_id FROM policy_roles pr
				WHERE pr.role_id IN (` + reachableRolesCTE + `)
			)
		  )
		ORDER BY a.created_at, a.id`
	return r.agentIDs(ctx, q, memberID, orgID)
}

// agentIDs runs a two-parameter ($1 member, $2 org) agent-id query and collects
// the ids.
func (r *AccessResolver) agentIDs(ctx context.Context, q, memberID, orgID string) ([]string, error) {
	rows, err := r.db.Query(ctx, q, memberID, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list accessible agents for member %q: %w", memberID, mapError(err))
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: list accessible agents for member %q: %w", memberID, mapError(err))
	}
	return ids, nil
}

// hasAllAgents reports whether any role reachable by memberID grants all agents.
func (r *AccessResolver) hasAllAgents(ctx context.Context, memberID string) (bool, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM roles r
		WHERE r.all_agents AND r.id IN (` + reachableRolesCTE + `)
	)`
	var ok bool
	if err := r.db.QueryRow(ctx, q, memberID).Scan(&ok); err != nil {
		return false, mapError(err)
	}
	return ok, nil
}
