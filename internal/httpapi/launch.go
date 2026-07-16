package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/db"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/policycompile"
	"github.com/Domene99/drun-dash/internal/store"
	"github.com/Domene99/drun-dash/pkg/policy"
)

// bundleSchemaVersion is the pkg/policy bundle-format version stamped into every
// launched bundle. Its major must equal policy.SchemaMajor or the client's
// Load/Verify rejects the bundle.
const bundleSchemaVersion = "1.0"

// proxyTokenPrefix labels the raw proxy token for legibility ("drun grant
// proxy"), per ADR-0003 §3. It is part of the raw secret and of the hashed
// form stored in credential_grants.
const proxyTokenPrefix = "dgp_"

// proxyTokenBytes is the entropy of the raw proxy token (256 bits from
// crypto/rand), per ADR-0003 §3.
const proxyTokenBytes = 32

// The store seams the launch/terminate handlers resolve their reads through.
// Each is a package-level constructor var so tests can swap in an in-memory
// fake without a database, mirroring newAgentStore/newSessionStore. Production
// leaves them at their defaults, which build the real store over the request's
// pool (resolution in ADR-0003 §1 is read-only, so these run on d.Pool, not a
// transaction).
//
// agentVersionStore and entitlementStore are shared with the agent catalog
// handlers (agents.go): those add the version-history read (ListByAgent) and the
// entitlement replace (Create/Delete) the launch path does not itself use, so
// the interfaces carry a superset of the launch handlers' needs.

type agentVersionStore interface {
	GetByAgentAndVersion(ctx context.Context, agentID string, version int) (store.AgentVersion, error)
	ListByAgent(ctx context.Context, agentID string) ([]store.AgentVersion, error)
}

var newAgentVersionStore = func(q store.Querier) agentVersionStore {
	return store.NewAgentVersionStore(q)
}

type entitlementStore interface {
	ListByAgent(ctx context.Context, agentID string) ([]store.Entitlement, error)
	Create(ctx context.Context, p store.CreateEntitlementParams) (store.Entitlement, error)
	Delete(ctx context.Context, id string) error
}

var newEntitlementStore = func(q store.Querier) entitlementStore {
	return store.NewEntitlementStore(q)
}

type teamMembershipReader interface {
	ListTeams(ctx context.Context, memberID string) ([]store.Team, error)
}

var newTeamMembershipStore = func(q store.Querier) teamMembershipReader {
	return store.NewTeamMembershipStore(q)
}

// policyVersionStore is the shared slice of *[store.PolicyVersionStore] the
// httpapi handlers use. launchSession only reads a pinned version back
// (GetByID); the policy handlers additionally append versions (Create) and read
// a policy's history (ListByPolicy). Keeping one seam avoids two package-level
// factories over the same store — each handler simply calls the methods it
// needs.
type policyVersionStore interface {
	GetByID(ctx context.Context, id string) (store.PolicyVersion, error)
	Create(ctx context.Context, p store.CreatePolicyVersionParams) (store.PolicyVersion, error)
	ListByPolicy(ctx context.Context, policyID string) ([]store.PolicyVersion, error)
}

var newPolicyVersionStore = func(q store.Querier) policyVersionStore {
	return store.NewPolicyVersionStore(q)
}

// sessionLaunchWriter performs the ADR-0003 §4 atomic mutation: it inserts the
// session and its credential grant in one transaction, so a token is never
// handed out without a durable, enforceable grant (and a session never exists
// without its credential). Persist stamps the created session's id onto the
// grant params before inserting the grant.
type sessionLaunchWriter interface {
	Persist(ctx context.Context, sp store.CreateSessionParams, gp store.CreateCredentialGrantParams) (store.Session, store.CredentialGrant, error)
}

var newSessionLaunchWriter = func(pool *db.Pool) sessionLaunchWriter {
	return dbSessionLaunchWriter{pool: pool}
}

// dbSessionLaunchWriter is the production sessionLaunchWriter. It owns the
// pgx.Tx entirely — the stores accept any store.Querier, so the transaction is
// passed straight in — keeping the transactional plumbing out of the handler
// and out of the tests.
type dbSessionLaunchWriter struct{ pool *db.Pool }

func (w dbSessionLaunchWriter) Persist(ctx context.Context, sp store.CreateSessionParams, gp store.CreateCredentialGrantParams) (store.Session, store.CredentialGrant, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return store.Session{}, store.CredentialGrant{}, err
	}
	// Rollback is a no-op once Commit has run; on any early return it undoes the
	// partial write so neither the session nor the grant is persisted.
	defer func() { _ = tx.Rollback(ctx) }()

	session, err := store.NewSessionStore(tx).Create(ctx, sp)
	if err != nil {
		return store.Session{}, store.CredentialGrant{}, err
	}
	gp.SessionID = session.ID
	grant, err := store.NewCredentialGrantStore(tx).Create(ctx, gp)
	if err != nil {
		return store.Session{}, store.CredentialGrant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Session{}, store.CredentialGrant{}, err
	}
	return session, grant, nil
}

// sessionTerminator performs the ADR-0003 §3 termination mutation: it revokes
// every grant minted for the session and sets the session status to
// terminated, in one transaction. It is idempotent — revoking an
// already-revoked grant re-stamps revoked_at and re-terminating re-runs the
// status update — so terminating a terminal session still returns the row.
type sessionTerminator interface {
	Terminate(ctx context.Context, sessionID string) (store.Session, error)
}

var newSessionTerminator = func(pool *db.Pool) sessionTerminator {
	return dbSessionTerminator{pool: pool}
}

type dbSessionTerminator struct{ pool *db.Pool }

func (t dbSessionTerminator) Terminate(ctx context.Context, sessionID string) (store.Session, error) {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return store.Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	grants := store.NewCredentialGrantStore(tx)
	existing, err := grants.ListBySession(ctx, sessionID)
	if err != nil {
		return store.Session{}, err
	}
	for _, g := range existing {
		if _, err := grants.Revoke(ctx, g.ID); err != nil {
			return store.Session{}, err
		}
	}
	session, err := store.NewSessionStore(tx).SetStatus(ctx, sessionID, store.SessionStatusTerminated)
	if err != nil {
		return store.Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Session{}, err
	}
	return session, nil
}

// apiSessionLaunchRequest is the POST /sessions/launch body
// (openapi/v1.yaml #/components/schemas/SessionLaunchRequest). Only agent_id is
// required; agent_version pins a specific version (else the agent's current
// version is used), requested_fan_out is clamped down to the policy/entitlement
// ceilings, and labels are recorded for attribution.
type apiSessionLaunchRequest struct {
	AgentID         string            `json:"agent_id"`
	AgentVersion    *int              `json:"agent_version"`
	RequestedFanOut *int              `json:"requested_fan_out"`
	Labels          map[string]string `json:"labels"`
}

// apiSessionLaunchResponse is the 201 body
// (openapi/v1.yaml #/components/schemas/SessionLaunchResponse).
//
// policy_bundle carries the full, verifiable pkg/policy.PolicyBundle rather than
// the contract's leaner PolicyBundle shape: ADR-0003 §2 decides the client must
// receive a bundle it can run policy.Load/Verify against (schema_version,
// drun_config, issued_at/expires_at, content_hash, signature), which the
// OpenAPI PolicyBundle omits. Extending the contract to carry this shape is the
// named follow-up recorded in the ADR; the honest artifact ships now.
type apiSessionLaunchResponse struct {
	SessionID     string              `json:"session_id"`
	PolicyBundle  policy.PolicyBundle `json:"policy_bundle"`
	ProxyBaseURL  string              `json:"proxy_base_url"`
	ProxyToken    string              `json:"proxy_token"`
	FanOutCeiling int                 `json:"fan_out_ceiling"`
	ExpiresAt     *time.Time          `json:"expires_at,omitempty"`
}

// grantScopes is the JSON structure stored in credential_grants.scopes: what
// the grant authorises at the proxy. It is derived from policy (the pinned
// endpoint and the operations the boundary permits), never from the client, so
// a stolen token cannot widen its own scope (ADR-0003 §3).
type grantScopes struct {
	EndpointID string   `json:"endpoint_id"`
	Operations []string `json:"operations"`
}

// launchSession implements POST /sessions/launch (operationId launchSession).
// It executes ADR-0003 §1–§5: resolve the agent version + role-based authorization (all from
// the token principal), compile and sign the policy bundle, mint a scoped proxy
// grant, persist the session and grant atomically, and return the bundle plus
// the raw proxy token exactly once.
func (d Deps) launchSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	var req apiSessionLaunchRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.AgentID == "" {
		WriteValidationError(w, "agent_id", "agent_id is required")
		return
	}
	if req.RequestedFanOut != nil && *req.RequestedFanOut < 0 {
		WriteValidationError(w, "requested_fan_out", "requested_fan_out must be >= 0")
		return
	}

	// A missing bundle signing key is a control-plane misconfiguration, not a
	// client error: fail closed rather than mint a session without a verifiable
	// bundle.
	if len(d.BundleSigningKey) != ed25519.PrivateKeySize {
		slog.Error("session launch: bundle signing key not configured")
		writeInternalError(w)
		return
	}

	// §1.1 Load the agent, org-scoped. A cross-org or missing agent is a 404 so
	// nothing leaks across tenants.
	agent, err := newAgentStore(d.Pool).GetByID(ctx, req.AgentID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, agent.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	// §1.2 Select the agent version.
	av, ok := d.resolveAgentVersion(w, ctx, agent, req.AgentVersion)
	if !ok {
		return
	}

	// §1.3 Authorize the launch against the caller's roles (ADR-0006 §6): the
	// caller must hold agents.run in their effective permission set AND this
	// agent must be in their effective agent access. No match → the one
	// contract-sanctioned 403.
	grant, ok := d.authorizeRun(w, ctx, agent.ID, claims.MemberID)
	if !ok {
		return
	}

	// §1.4 Effective policy version: the resolved run grant's override wins,
	// else the agent version's pin. Neither present is a misconfiguration → 500.
	var policyVersionID string
	switch {
	case grant.PolicyVersionID != nil:
		policyVersionID = *grant.PolicyVersionID
	case av.PolicyVersionID != nil:
		policyVersionID = *av.PolicyVersionID
	default:
		slog.Error("session launch: no effective policy version",
			slog.String("agent_id", agent.ID), slog.Int("agent_version", av.Version))
		writeInternalError(w)
		return
	}

	// §1.5 Resolve the endpoint the proxy will vault.Resolve at call time.
	if av.EndpointID == nil {
		slog.Error("session launch: agent version has no endpoint",
			slog.String("agent_id", agent.ID), slog.Int("agent_version", av.Version))
		writeInternalError(w)
		return
	}
	endpointID := *av.EndpointID

	// §2 Compile the stored boundary into an enforceable drun_config.
	pv, err := newPolicyVersionStore(d.Pool).GetByID(ctx, policyVersionID)
	if err != nil {
		slog.Error("session launch: load policy version", slog.String("policy_version_id", policyVersionID), slog.Any("error", err))
		writeInternalError(w)
		return
	}
	boundary, err := policycompile.ParseBoundary(pv.ContentJSON)
	if err != nil {
		slog.Error("session launch: parse boundary", slog.String("policy_version_id", policyVersionID), slog.Any("error", err))
		writeInternalError(w)
		return
	}
	drunConfig, err := policycompile.Compile(boundary)
	if err != nil {
		slog.Error("session launch: compile boundary", slog.String("policy_version_id", policyVersionID), slog.Any("error", err))
		writeInternalError(w)
		return
	}

	// §1 fan-out clamp: the minimum of every bound that applies.
	fanOut := clampFanOut(av.FanOutCeiling, req.RequestedFanOut, boundary.FanOutCeiling, grant.MaxFanOut)

	// §2 Assemble and sign the bundle before any mutation.
	now := time.Now().UTC()
	expiresAt := now.Add(d.SessionTTL)
	bundle, err := d.signBundle(claims.OrgID, pv, drunConfig, now, expiresAt)
	if err != nil {
		slog.Error("session launch: sign bundle", slog.String("policy_version_id", policyVersionID), slog.Any("error", err))
		writeInternalError(w)
		return
	}

	// §3 Mint the scoped proxy grant (raw token returned once, only its hash
	// stored).
	rawToken, err := mintProxyToken()
	if err != nil {
		slog.Error("session launch: mint proxy token", slog.Any("error", err))
		writeInternalError(w)
		return
	}
	scopesJSON, err := json.Marshal(grantScopes{EndpointID: endpointID, Operations: []string{"chat.completions"}})
	if err != nil {
		slog.Error("session launch: marshal scopes", slog.Any("error", err))
		writeInternalError(w)
		return
	}

	var labelsJSON json.RawMessage
	if len(req.Labels) > 0 {
		labelsJSON, err = json.Marshal(req.Labels)
		if err != nil {
			slog.Error("session launch: marshal labels", slog.Any("error", err))
			writeInternalError(w)
			return
		}
	}

	// §4 Persist the session + grant atomically.
	agentVersion := av.Version
	var launchedBy *string
	if claims.MemberID != "" {
		mid := claims.MemberID
		launchedBy = &mid
	}
	session, _, err := newSessionLaunchWriter(d.Pool).Persist(ctx,
		store.CreateSessionParams{
			OrgID:           claims.OrgID,
			AgentID:         agent.ID,
			AgentVersion:    &agentVersion,
			PolicyVersionID: &policyVersionID,
			LaunchedBy:      launchedBy,
			Status:          store.SessionStatusActive,
			FanOutCeiling:   fanOut,
			Labels:          labelsJSON,
		},
		store.CreateCredentialGrantParams{
			ProxyTokenHash: sha256Hex(rawToken),
			EndpointID:     &endpointID,
			Scopes:         scopesJSON,
			ExpiresAt:      expiresAt,
		},
	)
	if err != nil {
		slog.Error("session launch: persist session and grant", slog.String("agent_id", agent.ID), slog.Any("error", err))
		WriteStoreError(w, err)
		return
	}

	// §5 Respond. expires_at reports the single session lifetime the bundle and
	// grant share.
	WriteCreated(w, apiSessionLaunchResponse{
		SessionID:     session.ID,
		PolicyBundle:  bundle,
		ProxyBaseURL:  d.ProxyBaseURL,
		ProxyToken:    rawToken,
		FanOutCeiling: fanOut,
		ExpiresAt:     &expiresAt,
	})
}

// resolveAgentVersion selects the agent version per ADR-0003 §1.2. A pinned
// version that does not exist is a 404; the current-version path is a 404 only
// when the agent has never published (current_version 0) and otherwise treats a
// miss as a server-side misconfiguration (500). On any failure it writes the
// response and returns ok=false.
func (d Deps) resolveAgentVersion(w http.ResponseWriter, ctx context.Context, agent store.Agent, pin *int) (store.AgentVersion, bool) {
	versions := newAgentVersionStore(d.Pool)
	if pin != nil {
		av, err := versions.GetByAgentAndVersion(ctx, agent.ID, *pin)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				WriteNotFound(w, "resource not found")
			} else {
				slog.Error("session launch: load pinned agent version", slog.String("agent_id", agent.ID), slog.Int("version", *pin), slog.Any("error", err))
				writeInternalError(w)
			}
			return store.AgentVersion{}, false
		}
		return av, true
	}

	if agent.CurrentVersion == 0 {
		WriteNotFound(w, "agent has no published version to launch")
		return store.AgentVersion{}, false
	}
	av, err := versions.GetByAgentAndVersion(ctx, agent.ID, agent.CurrentVersion)
	if err != nil {
		// current_version points at a version that must exist; a miss here is a
		// data-integrity fault, not a client error.
		slog.Error("session launch: load current agent version", slog.String("agent_id", agent.ID), slog.Int("current_version", agent.CurrentVersion), slog.Any("error", err))
		writeInternalError(w)
		return store.AgentVersion{}, false
	}
	return av, true
}

// signBundle assembles a pkg/policy.PolicyBundle for the resolved policy version
// and compiled config, computes its content hash over the canonical drun_config,
// and signs the bundle's SigningInput with the control plane's Ed25519
// bundle-signing key (ADR-0003 §2). The signing mechanics mirror
// cmd/policyctl/sign.go.
func (d Deps) signBundle(orgID string, pv store.PolicyVersion, cfg policy.DrunConfig, issuedAt, expiresAt time.Time) (policy.PolicyBundle, error) {
	rawConfig, err := json.Marshal(cfg)
	if err != nil {
		return policy.PolicyBundle{}, err
	}
	contentHash, err := policy.ComputeContentHash(rawConfig)
	if err != nil {
		return policy.PolicyBundle{}, err
	}
	bundle := policy.PolicyBundle{
		SchemaVersion: bundleSchemaVersion,
		OrgID:         orgID,
		PolicyID:      pv.PolicyID,
		PolicyVersion: pv.Version,
		DrunConfig:    cfg,
		IssuedAt:      issuedAt.Format(time.RFC3339),
		ExpiresAt:     expiresAt.Format(time.RFC3339),
		ContentHash:   contentHash,
	}
	sig := ed25519.Sign(d.BundleSigningKey, bundle.SigningInput())
	bundle.Signature = "ed25519:" + base64.StdEncoding.EncodeToString(sig)
	return bundle, nil
}

// terminateSession implements POST /sessions/{sessionId}/terminate (operationId
// terminateSession). It loads the session org-scoped (a cross-org or missing id
// is a 404), then revokes every grant and sets the status to terminated in one
// transaction. It is idempotent: terminating an already-terminal session
// re-stamps and returns 200 (ADR-0003 §3).
func (d Deps) terminateSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "sessionId")

	session, err := newSessionStore(d.Pool).GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, session.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	updated, err := newSessionTerminator(d.Pool).Terminate(ctx, id)
	if err != nil {
		slog.Error("session terminate", slog.String("session_id", id), slog.Any("error", err))
		WriteStoreError(w, err)
		return
	}
	WriteOK(w, toAPISession(updated))
}

// clampFanOut returns the effective fan-out ceiling: the minimum of the agent
// version's ceiling (always a bound) and any of the requested value, the policy
// boundary's ceiling, and the run grant's max_fan_out that are set. A
// requested value only ever clamps the ceiling down (ADR-0003 §1). The result
// is floored at 0 to satisfy the sessions.fan_out_ceiling CHECK (>= 0).
func clampFanOut(agentVersionCeiling int, bounds ...*int) int {
	eff := agentVersionCeiling
	for _, b := range bounds {
		if b != nil && *b < eff {
			eff = *b
		}
	}
	if eff < 0 {
		eff = 0
	}
	return eff
}

// mintProxyToken generates the raw proxy token: proxyTokenPrefix + base64url of
// 32 crypto/rand bytes (ADR-0003 §3). The raw value is returned to the client
// exactly once and never persisted.
func mintProxyToken() (string, error) {
	buf := make([]byte, proxyTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return proxyTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// sha256Hex returns the lowercase-hex SHA-256 of s — the only stored form of
// the proxy token. The token is 256 bits of crypto/rand entropy, so a fast hash
// is correct here (nothing to brute-force) and keeps the unique-index lookup
// exact (ADR-0003 §3).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// writeInternalError writes the generic 500 used for server-side faults
// (misconfiguration, marshalling, signing), never echoing internals to the
// client.
func writeInternalError(w http.ResponseWriter) {
	_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
}
