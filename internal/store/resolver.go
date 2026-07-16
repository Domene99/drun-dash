package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AccessResolver answers the effective-access questions the enforcement layer
// asks per request (ADR-0006 §4/§6). It resolves a member's grants from the
// database on every call — never from a token claim — so revocation is prompt
// and fail-closed. Effective access is the union across every role reachable
// directly (member_roles) or via a team (team_roles ∘ team_memberships).
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

// RunGrant is the resolved outcome of an authorized launch (ADR-0006 §4). When
// AllAgents is true the member reached the agent through an "all agents" role
// and no per-agent tuning applies (the agent version's own pins stand).
// Otherwise PolicyVersionID / MaxFanOut carry the effective override drawn from
// the matched role_agents grants, or nil when the agent version's pin should
// stand.
type RunGrant struct {
	AllAgents       bool
	PolicyVersionID *string
	MaxFanOut       *int
}

// AuthorizeRun decides whether memberID may launch agentID and, if so, returns
// the effective launch tuning (ADR-0006 §4/§6). A member may launch iff they
// hold agents.run in their effective permission set AND the agent is in their
// effective agent access ("all agents" on any reachable role, or an explicit
// role_agents grant). It replaces launch.go's resolveEntitlement.
//
// When several grants reach the same agent, the most permissive tuning wins:
// the effective MaxFanOut is the maximum across grants (a nil/unbounded grant is
// the most permissive and wins), and a PolicyVersionID override applies only
// when every reaching grant agrees on the same override, otherwise it is dropped
// and the agent version's own pin stands. The agent-version and policy ceilings
// still clamp the result downstream (launch.go clampFanOut).
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

	grants, err := r.explicitGrants(ctx, memberID, agentID)
	if err != nil {
		return RunGrant{}, false, fmt.Errorf("store: authorize run of agent %q by member %q: %w", agentID, memberID, err)
	}
	if len(grants) == 0 {
		return RunGrant{}, false, nil
	}
	return resolveRunGrant(grants), true, nil
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

// explicitGrants returns every role_agents grant for agentID reachable by
// memberID (ignoring all_agents roles, handled separately).
func (r *AccessResolver) explicitGrants(ctx context.Context, memberID, agentID string) ([]RoleAgent, error) {
	const q = `SELECT ra.role_id, ra.agent_id, ra.policy_version_id, ra.max_fan_out
		FROM role_agents ra
		WHERE ra.agent_id = $2 AND ra.role_id IN (` + reachableRolesCTE + `)`
	rows, err := r.db.Query(ctx, q, memberID, agentID)
	if err != nil {
		return nil, mapError(err)
	}
	grants, err := pgx.CollectRows(rows, scanRoleAgentRow)
	if err != nil {
		return nil, mapError(err)
	}
	return grants, nil
}

// resolveRunGrant folds several grants for one agent into the effective tuning
// per ADR-0006 §4 (see AuthorizeRun).
func resolveRunGrant(grants []RoleAgent) RunGrant {
	g := RunGrant{}

	// MaxFanOut: the maximum across grants; a nil grant means "no ceiling" and
	// is the most permissive, so it wins outright.
	maxFanOut := -1
	fanOutUnbounded := false
	for _, x := range grants {
		if x.MaxFanOut == nil {
			fanOutUnbounded = true
			break
		}
		if *x.MaxFanOut > maxFanOut {
			maxFanOut = *x.MaxFanOut
		}
	}
	if !fanOutUnbounded && maxFanOut >= 0 {
		v := maxFanOut
		g.MaxFanOut = &v
	}

	// PolicyVersionID: applies only when every grant agrees on one non-nil
	// override; any disagreement (differing values, or a mix of set and unset)
	// drops it so the agent version's own pin stands.
	var agreed *string
	consistent := true
	for i, x := range grants {
		if i == 0 {
			agreed = x.PolicyVersionID
			continue
		}
		if !samePtr(agreed, x.PolicyVersionID) {
			consistent = false
			break
		}
	}
	if consistent && agreed != nil {
		v := *agreed
		g.PolicyVersionID = &v
	}
	return g
}

// samePtr reports whether two optional strings hold the same value (both nil, or
// both non-nil and equal).
func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
