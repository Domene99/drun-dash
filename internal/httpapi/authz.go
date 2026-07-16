package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// accessResolver is the narrow slice of *[store.AccessResolver] the
// authorization layer depends on: the per-request permission check the
// RequirePermission gate runs (ADR-0006 §6), the effective-permission set
// /auth/whoami surfaces (§7), and the agent-scoped run check launchSession uses
// (§6). Naming it as an interface lets the tests inject an in-memory fake
// without a database, exactly like the other store seams.
type accessResolver interface {
	HasPermission(ctx context.Context, memberID string, perm store.Permission) (bool, error)
	EffectivePermissions(ctx context.Context, memberID string) ([]store.Permission, error)
	AuthorizeRun(ctx context.Context, memberID, agentID string) (store.RunGrant, bool, error)
}

// newAccessResolver builds the [accessResolver] the authorization layer
// resolves through. Like the other store seams it is a package-level var so
// tests can swap in a fake without a database; production constructs the real
// resolver over the request's pool. Authorization is resolved from the database
// per request (never from the JWT roles claim), so a revoked role takes effect
// on the next call — the fail-closed, prompt-revocation posture of ADR-0006 §6.
var newAccessResolver = func(q store.Querier) accessResolver {
	return store.NewAccessResolver(q)
}

// requirePermission returns middleware that authorizes the authenticated
// principal against perm before invoking next (ADR-0006 §6). It composes behind
// authmw.RequireAuth (which runs first), so a principal is always present: it
// reads the principal, resolves the caller's effective permission set from the
// database, and calls next iff perm is present, otherwise writes 403 forbidden.
// It fails closed — a principal without a member id (a roles-only token) is
// denied, and a resolver error denies the request (a 500) rather than proceeding.
func (d Deps) requirePermission(perm store.Permission) httpx.Middleware {
	return d.requireAnyPermission(perm)
}

// requireAnyPermission is requirePermission generalised to a set: the caller
// passes iff their effective permissions include ANY of perms. It backs the one
// endpoint ADR-0006 §6 makes visible under more than one permission — GET
// /members, visible to members.manage OR audit.view.
func (d Deps) requireAnyPermission(perms ...store.Permission) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			claims := authmw.MustPrincipal(ctx)
			// A roles-only token carries no member to resolve grants for; fail
			// closed rather than treat an absent principal as permitted.
			if claims.MemberID == "" {
				WriteForbidden(w, "forbidden")
				return
			}
			resolver := newAccessResolver(d.Pool)
			for _, perm := range perms {
				ok, err := resolver.HasPermission(ctx, claims.MemberID, perm)
				if err != nil {
					slog.Error("authorize: resolve permission",
						slog.String("member_id", claims.MemberID),
						slog.String("permission", string(perm)),
						slog.Any("error", err))
					writeInternalError(w)
					return
				}
				if ok {
					next.ServeHTTP(w, r)
					return
				}
			}
			WriteForbidden(w, "forbidden")
		})
	}
}

// authorizeRun performs the agent-scoped launch check (ADR-0006 §6): the caller
// may launch agentID iff they hold agents.run in their effective permission set
// AND the agent is in their effective agent access. It replaces launch.go's
// resolveEntitlement. On success it returns the resolved run grant — the
// effective policy-version override and fan-out ceiling the matched role_agents
// supply, which ADR-0003 §1.4/clampFanOut consume unchanged. On a miss it writes
// the one contract-sanctioned 403 and returns ok=false; a resolver error is a
// 500 (never a silent unbounded launch).
func (d Deps) authorizeRun(w http.ResponseWriter, ctx context.Context, agentID, memberID string) (store.RunGrant, bool) {
	grant, ok, err := newAccessResolver(d.Pool).AuthorizeRun(ctx, memberID, agentID)
	if err != nil {
		slog.Error("session launch: authorize run",
			slog.String("agent_id", agentID),
			slog.String("member_id", memberID),
			slog.Any("error", err))
		writeInternalError(w)
		return store.RunGrant{}, false
	}
	if !ok {
		WriteForbidden(w, "not entitled to launch this agent")
		return store.RunGrant{}, false
	}
	return grant, true
}
