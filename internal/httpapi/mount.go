package httpapi

import (
	"context"
	"crypto/ed25519"
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
// one line in [Mount] — no change to this struct or to cmd/control-plane. The
// session-launch fields below are the exception: signing a policy bundle and
// scoping a grant need process-wide material (a key, the proxy URL, the session
// lifetime) that is not derivable from the request, so they are wired once here.
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
	// Redis backs the device-authorization flow state (ADR-0001 §2): the
	// pending-authorization records keyed by device_code hash and user_code.
	// Only the device handlers use it; tests inject an in-memory DeviceStore.
	Redis DeviceStore
	// AdminUIBaseURL is the admin UI origin used to build the device-flow
	// verification_uri (<AdminUIBaseURL>/device) the CLI shows the user.
	AdminUIBaseURL string
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

	// BundleSigningKey is the control plane's Ed25519 private key for signing
	// policy bundles at session launch (ADR-0003 §2). It is distinct from the
	// JWT session-signing key (different purpose and rotation) and its public
	// half is what proxies and clients verify against. launchSession fails
	// closed with a 500 when it is not configured.
	BundleSigningKey ed25519.PrivateKey
	// BundleSigningKID identifies the bundle-signing key for rotation. It is
	// not embedded in the pkg/policy bundle (which carries no key id) but is
	// tracked here for future key distribution.
	BundleSigningKID string
	// ProxyBaseURL is the LLM proxy every launched session must route through;
	// it is echoed verbatim as SessionLaunchResponse.proxy_base_url.
	ProxyBaseURL string
	// SessionTTL is the single lifetime that governs both the signed bundle's
	// expires_at and the credential grant's expires_at (ADR-0003 §4), so a
	// session cannot outlive its policy or vice versa.
	SessionTTL time.Duration
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

	// The device-authorization flow's public endpoints (RFC 8628): the CLI has
	// no Bearer token yet, so both the code request and the token poll are
	// unauthenticated. Approval (below) is the one step that requires a member.
	r.Post("/auth/device/code", deps.startDeviceAuthorization)
	r.Post("/auth/device/token", deps.pollDeviceToken)

	// Everything below requires a valid access token. Grouping keeps the
	// middleware scoped to protected routes without affecting the public ones.
	r.Group(func(pr chi.Router) {
		pr.Use(authmw.RequireAuth(deps.Verifier))
		pr.Get("/auth/whoami", deps.getCurrentIdentity)

		// Device-flow approval is the admin-UI→control-plane extension: the
		// member approves or denies a pending CLI authorization from the
		// browser session established by OIDC login. Not in openapi/v1.yaml.
		pr.Post("/auth/device/approve", deps.approveDevice)

		pr.Get("/agents", deps.listAgents)
		pr.Post("/agents", deps.createAgent)
		pr.Get("/agents/{agentId}", deps.getAgent)
		pr.Patch("/agents/{agentId}", deps.updateAgent)
		pr.Delete("/agents/{agentId}", deps.deleteAgent)
		pr.Get("/agents/{agentId}/versions", deps.listAgentVersions)
		pr.Post("/agents/{agentId}/versions", deps.publishAgentVersion)
		pr.Get("/agents/{agentId}/entitlements", deps.listAgentEntitlements)
		pr.Put("/agents/{agentId}/entitlements", deps.setAgentEntitlements)

		pr.Get("/policies", deps.listPolicies)
		pr.Post("/policies", deps.createPolicy)
		pr.Get("/policies/{policyId}", deps.getPolicy)
		pr.Get("/policies/{policyId}/versions", deps.listPolicyVersions)
		pr.Post("/policies/{policyId}/versions", deps.publishPolicyVersion)

		pr.Get("/sessions", deps.listSessions)
		pr.Post("/sessions/launch", deps.launchSession)
		pr.Get("/sessions/{sessionId}", deps.getSession)
		pr.Post("/sessions/{sessionId}/terminate", deps.terminateSession)
		pr.Post("/sessions/{sessionId}/trajectory", deps.ingestTrajectoryEvents)
		pr.Get("/sessions/{sessionId}/trajectory", deps.listTrajectoryEvents)

		pr.Get("/approvals", deps.listApprovals)
		pr.Post("/approvals", deps.createApproval)
		pr.Post("/approvals/{approvalId}/decision", deps.decideApproval)

		pr.Get("/analytics/spend", deps.querySpend)
		pr.Get("/analytics/budgets", deps.listBudgets)
	})
}
