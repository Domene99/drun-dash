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
	// OIDC verifies the IdP-issued ID token presented to POST /auth/session. It
	// is optional: cmd/control-plane leaves it nil when the IdP is unconfigured
	// or was unreachable at boot, in which case exchangeOidcSession returns 501
	// and the rest of the API is unaffected (it needs only the Verifier).
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
	// verification_uri (<AdminUIBaseURL>/device) the CLI shows the user, and the
	// accept-invite link (<AdminUIBaseURL>/accept-invite?token=…) returned by
	// POST /members/invite (ADR-0008 §6).
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
// Mutating and org-wide-audit routes additionally pass through a
// [Deps.requirePermission] gate (ADR-0006 §6), which resolves the caller's
// effective permissions from the database per request and answers 403 when the
// required permission is absent. Read routes stay member-visible within the org
// (only querySpend, an org-wide audit surface, is gated, on audit.view).
//
// This is the single seam the binary calls to expose the API surface: the
// authentication vertical (POST /auth/session, GET /auth/whoami) plus the
// resource handlers. Adding a resource is one line in the protected group here.
// The whole surface is always mounted — it depends only on the always-built
// auth stack, not on OIDC — so an unconfigured IdP degrades just the session
// exchange (to 501), never the availability of the API.
func Mount(r chi.Router, deps Deps) {
	// POST /auth/session is unauthenticated by design: it is how a caller
	// obtains a token in the first place (it authenticates via the OIDC ID
	// token in the body, not a Bearer header). When OIDC is not configured it
	// returns 501; it stays mounted regardless.
	r.Post("/auth/session", deps.exchangeOidcSession)

	// Local email/password auth (ADR-0008), all unauthenticated: signup creates
	// an org and its Owner, login trades (org_slug, email, password) for a
	// token, and accept-invite redeems an invitation. Each mints the same
	// session JWT as the OIDC exchange, so everything downstream is unchanged.
	r.Post("/auth/signup", deps.signup)
	r.Post("/auth/login", deps.login)
	r.Post("/auth/accept-invite", deps.acceptInvite)

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

		// GET /me/agents returns only the caller's entitled agents (the member
		// console surface), distinct from GET /agents (all org agents). A
		// documented contract extension, not in openapi/v1.yaml.
		pr.Get("/me/agents", deps.listMyAgents)

		// Agents. Reads are member-visible within the org; mutations require
		// agents.manage (ADR-0006 §6). setAgentEntitlements is the legacy grant
		// surface superseded by role-agent management, so it requires
		// roles.manage.
		pr.Get("/agents", deps.listAgents)
		pr.With(deps.requirePermission(store.PermAgentsManage)).Post("/agents", deps.createAgent)
		pr.Get("/agents/{agentId}", deps.getAgent)
		pr.With(deps.requirePermission(store.PermAgentsManage)).Patch("/agents/{agentId}", deps.updateAgent)
		pr.With(deps.requirePermission(store.PermAgentsManage)).Delete("/agents/{agentId}", deps.deleteAgent)
		pr.Get("/agents/{agentId}/versions", deps.listAgentVersions)
		pr.With(deps.requirePermission(store.PermAgentsManage)).Post("/agents/{agentId}/versions", deps.publishAgentVersion)
		pr.Get("/agents/{agentId}/entitlements", deps.listAgentEntitlements)
		pr.With(deps.requirePermission(store.PermRolesManage)).Put("/agents/{agentId}/entitlements", deps.setAgentEntitlements)

		// Policies. Reads member-visible; create/publish require policies.manage.
		pr.Get("/policies", deps.listPolicies)
		pr.With(deps.requirePermission(store.PermPoliciesManage)).Post("/policies", deps.createPolicy)
		pr.Get("/policies/{policyId}", deps.getPolicy)
		pr.Get("/policies/{policyId}/versions", deps.listPolicyVersions)
		pr.With(deps.requirePermission(store.PermPoliciesManage)).Post("/policies/{policyId}/versions", deps.publishPolicyVersion)

		// Roles and members (ADR-0006 §7). Defining a role is roles.manage;
		// assigning roles to a member and inviting members is members.manage;
		// the member roster is visible to members.manage OR audit.view.
		pr.With(deps.requirePermission(store.PermRolesManage)).Get("/roles", deps.listRoles)
		pr.With(deps.requirePermission(store.PermRolesManage)).Post("/roles", deps.createRole)
		pr.With(deps.requirePermission(store.PermRolesManage)).Get("/roles/{roleId}", deps.getRole)
		pr.With(deps.requirePermission(store.PermRolesManage)).Patch("/roles/{roleId}", deps.updateRole)
		pr.With(deps.requirePermission(store.PermRolesManage)).Delete("/roles/{roleId}", deps.deleteRole)
		pr.With(deps.requireAnyPermission(store.PermMembersManage, store.PermAuditView)).Get("/members", deps.listMembers)
		pr.With(deps.requirePermission(store.PermMembersManage)).Post("/members/invite", deps.inviteMember)
		pr.With(deps.requirePermission(store.PermMembersManage)).Put("/members/{memberId}/roles", deps.updateMemberRoles)

		// Sessions. Launch is agent-scoped (agents.run + agent access, checked
		// in the handler); terminate is the launcher-or-members.manage rule
		// (checked in the handler). listSessions/getSession stay member-visible.
		pr.Get("/sessions", deps.listSessions)
		pr.Post("/sessions/launch", deps.launchSession)
		pr.Get("/sessions/{sessionId}", deps.getSession)
		pr.Post("/sessions/{sessionId}/terminate", deps.terminateSession)
		pr.Post("/sessions/{sessionId}/trajectory", deps.ingestTrajectoryEvents)
		pr.Get("/sessions/{sessionId}/trajectory", deps.listTrajectoryEvents)

		// Approvals. Requesting review is a member action; deciding requires
		// approvals.decide.
		pr.Get("/approvals", deps.listApprovals)
		pr.Post("/approvals", deps.createApproval)
		pr.With(deps.requirePermission(store.PermApprovalsDecide)).Post("/approvals/{approvalId}/decision", deps.decideApproval)

		// Analytics. querySpend is an org-wide audit surface → audit.view;
		// listBudgets stays member-visible (ADR-0006 §6).
		pr.With(deps.requirePermission(store.PermAuditView)).Get("/analytics/spend", deps.querySpend)
		pr.Get("/analytics/budgets", deps.listBudgets)
	})
}
