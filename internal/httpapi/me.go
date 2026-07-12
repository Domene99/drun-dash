package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/store"
)

// meEntitlementReader is the slice of *[store.EntitlementStore] the /me/agents
// handler reads through: the caller's direct grants (ListByMember) and each of
// their teams' grants (ListByTeam). It is a separate seam from the
// launch/agents entitlementStore because those resolve grants by agent, while
// the member console resolves the grants attached to the caller's own
// principal. Keeping it narrow lets the test inject a fake tuned to the
// member/team reads without widening the by-agent fake.
type meEntitlementReader interface {
	ListByMember(ctx context.Context, memberID string) ([]store.Entitlement, error)
	ListByTeam(ctx context.Context, teamID string) ([]store.Entitlement, error)
}

// newMeEntitlementStore builds the [meEntitlementReader] the /me/agents handler
// reads through. Like the other store seams it is a package-level var so tests
// can swap in an in-memory fake; production constructs the real store over the
// request's pool.
var newMeEntitlementStore = func(q store.Querier) meEntitlementReader {
	return store.NewEntitlementStore(q)
}

// listMyAgents implements GET /me/agents. Unlike GET /agents (every agent in
// the org), this returns only the agents the calling member is entitled to —
// the surface the member console shows. It is a documented contract extension
// (like /auth/device/approve), not in openapi/v1.yaml.
//
// The entitled set is the union of the caller's direct member grants and the
// grants of every team they belong to (ADR-0003 §1.3 resolves launch the same
// way). Agent ids are collected distinct, then each agent is loaded and
// defensively org-scoped so the response can never include another tenant's
// agent even if a stray cross-org grant existed. The result reuses the AgentList
// envelope; the set is bounded by the caller's grants, so it is returned whole
// with a null cursor rather than paginated.
func (d Deps) listMyAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	// The member id is stamped on the access token at mint (see auth.go). A
	// token without it (roles-only) has no principal to resolve grants for, so
	// the entitled set is empty by definition.
	memberID := claims.MemberID
	if memberID == "" {
		WriteOK(w, apiAgentList{Items: []apiAgent{}, NextCursor: nil})
		return
	}

	ents := newMeEntitlementStore(d.Pool)

	// Collect the distinct entitled agent ids, preserving first-appearance
	// order (direct grants before team grants, each in the store's stable
	// order) for a deterministic response.
	var agentIDs []string
	seen := map[string]bool{}
	collect := func(list []store.Entitlement) {
		for _, e := range list {
			if !seen[e.AgentID] {
				seen[e.AgentID] = true
				agentIDs = append(agentIDs, e.AgentID)
			}
		}
	}

	direct, err := ents.ListByMember(ctx, memberID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	collect(direct)

	teams, err := newTeamMembershipStore(d.Pool).ListTeams(ctx, memberID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	for _, t := range teams {
		teamEnts, err := ents.ListByTeam(ctx, t.ID)
		if err != nil {
			WriteStoreError(w, err)
			return
		}
		collect(teamEnts)
	}

	// Load each entitled agent and defensively enforce tenant isolation: only
	// agents in the caller's org are returned (matching the rest of the agent
	// handlers). A grant pointing at an agent that no longer exists — which
	// should not happen, entitlements cascade on agent delete — is skipped
	// rather than failing the whole list.
	agents := newAgentStore(d.Pool)
	items := make([]apiAgent, 0, len(agentIDs))
	for _, id := range agentIDs {
		agent, err := agents.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			WriteStoreError(w, err)
			return
		}
		if !SameOrg(claims.OrgID, agent.OrgID) {
			continue
		}
		items = append(items, toAPIAgent(agent))
	}

	WriteOK(w, apiAgentList{Items: items, NextCursor: nil})
}
