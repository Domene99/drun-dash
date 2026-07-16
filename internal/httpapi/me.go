package httpapi

import (
	"errors"
	"net/http"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/store"
)

// listMyAgents implements GET /me/agents. Unlike GET /agents (every agent in
// the org), this returns only the agents the calling member may see and run —
// the surface the member console shows. It is a documented contract extension
// (like /auth/device/approve), not in openapi/v1.yaml.
//
// The visible set is resolved by the policy-centric access model (ADR-0010):
// the member sees an agent iff they hold agents.run AND (the agent's governing
// policy permits one of their reachable roles, OR the agent has no policy —
// general access, OR they hold an all_agents super-admin role). The resolver
// returns the accessible ids ordered by (created_at, id); each agent is then
// loaded and defensively org-scoped so the response can never include another
// tenant's agent. The result reuses the AgentList envelope; the set is bounded
// by the caller's access, so it is returned whole with a null cursor rather than
// paginated. It no longer reads the deprecated entitlements table.
func (d Deps) listMyAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	// The member id is stamped on the access token at mint (see auth.go). A
	// token without it (roles-only) has no principal to resolve access for, so
	// the visible set is empty by definition.
	memberID := claims.MemberID
	if memberID == "" {
		WriteOK(w, apiAgentList{Items: []apiAgent{}, NextCursor: nil})
		return
	}

	agentIDs, err := newAccessResolver(d.Pool).ListAccessibleAgentIDs(ctx, memberID, claims.OrgID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	// Load each accessible agent and defensively enforce tenant isolation: only
	// agents in the caller's org are returned (matching the rest of the agent
	// handlers). An id pointing at an agent that no longer exists — the resolver
	// and reads race a concurrent delete — is skipped rather than failing the
	// whole list.
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
