package httpapi

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/oidc"
	"github.com/Domene99/drun-dash/internal/store"
)

// MemberReader is the narrow member-lookup surface the auth handlers depend on.
// A *[store.MemberStore] satisfies it directly; tests inject a fake so the
// handlers can be exercised without a database. Later resource handlers that
// need more of the store surface add their own fields to [Deps] rather than
// widening this interface.
type MemberReader interface {
	// GetByIDPSubject resolves the member identified by idpSubject within
	// orgID, or returns [store.ErrNotFound].
	GetByIDPSubject(ctx context.Context, orgID, idpSubject string) (store.Member, error)
}

// Deps is the dependency set the control-plane REST handlers are built from. It
// is the seam between the wiring in cmd/control-plane and the handlers here:
// the binary assembles the auth stack and stores once, then hands them to
// [Mount], which registers the routes.
//
// Resource handlers (agents, policies, …) do not each add a field here: they
// build the store they need locally from [Deps.Pool] (e.g.
// store.NewAgentStore(d.Pool)), so adding a resource is a new handler file plus
// one line in [Mount] — no change to this struct or to cmd/control-plane.
type Deps struct {
	// OIDC verifies the IdP-issued ID token presented to POST /auth/session.
	OIDC oidc.Verifier
	// Issuer mints the control-plane access token returned by /auth/session.
	Issuer *auth.Issuer
	// Verifier validates access tokens; it backs the RequireAuth middleware
	// fronting the protected route group.
	Verifier *auth.Verifier
	// Members resolves the local member a verified OIDC subject maps to.
	Members MemberReader
	// Pool is the shared database pool the resource handlers build their
	// stores over (store.NewAgentStore(d.Pool), …). It is set once by
	// cmd/control-plane so new resources need not touch this struct or the
	// wiring. The auth handlers do not use it (they take the narrow Members
	// reader instead), so tests exercising only the auth vertical may leave it
	// nil.
	Pool *db.Pool
	// AccessTTL is the lifetime the issuer stamps into minted tokens; it is
	// reported verbatim as the TokenResponse.expires_in (in seconds). It must
	// match the TTL configured on Issuer.
	AccessTTL time.Duration
}

// Mount registers the control-plane REST routes on r. Public auth routes are
// mounted at the top level; the protected routes are wrapped in an isolated
// group that runs [authmw.RequireAuth] first, so every handler behind it can
// assume a principal is present.
//
// This is the single seam the binary calls to expose the API surface: the
// authentication vertical (POST /auth/session, GET /auth/whoami) plus the
// resource handlers. Adding a resource is one line in the protected group here.
func Mount(r chi.Router, deps Deps) {
	// POST /auth/session is unauthenticated by design: it is how a caller
	// obtains a token in the first place (it authenticates via the OIDC ID
	// token in the body, not a Bearer header).
	r.Post("/auth/session", deps.exchangeOidcSession)

	// Everything below requires a valid access token. Grouping keeps the
	// middleware scoped to protected routes without affecting the public ones.
	r.Group(func(pr chi.Router) {
		pr.Use(authmw.RequireAuth(deps.Verifier))
		pr.Get("/auth/whoami", deps.getCurrentIdentity)

		pr.Get("/agents", deps.listAgents)
		pr.Post("/agents", deps.createAgent)
		pr.Get("/agents/{agentId}", deps.getAgent)
	})
}
