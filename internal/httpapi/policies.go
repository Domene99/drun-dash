package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// policyStore is the narrow slice of *[store.PolicyStore] the policy handlers
// use. As with the other resources it is named as an interface so the tests can
// inject an in-memory fake in place of a live database while the production path
// runs the real SQL. SetCurrentVersion is included because a policy's current
// version pointer is advanced by both createPolicy (to 1) and
// publishPolicyVersion (to the next number).
type policyStore interface {
	Create(ctx context.Context, orgID, name string, description *string) (store.Policy, error)
	GetByID(ctx context.Context, id string) (store.Policy, error)
	ListByOrgPage(ctx context.Context, orgID string, limit int, afterCreatedAt time.Time, afterID string) ([]store.Policy, error)
	SetCurrentVersion(ctx context.Context, id string, version int) (store.Policy, error)
}

// newPolicyStore is the store-construction seam the policy handlers operate
// through — the same package-level-var pattern as newAgentStore, so tests swap
// in a fake without a database and production constructs the real store over the
// request's pool. The policy-version seam (newPolicyVersionStore, over the
// shared policyVersionStore interface) lives in launch.go, which also reads
// versions back at session launch; the policy handlers reuse it to append
// versions and read a policy's history.
var newPolicyStore = func(q store.Querier) policyStore {
	return store.NewPolicyStore(q)
}

// policyRoleStore is the narrow slice of *[store.PolicyRoleStore] the
// policy-roles handlers use (ADR-0010): read a policy's permitted roles and
// replace them wholesale. It is the same package-level-var seam so tests inject
// a fake without a database.
type policyRoleStore interface {
	ListRoles(ctx context.Context, policyID string) ([]string, error)
	SetRoles(ctx context.Context, policyID string, roleIDs []string) error
}

var newPolicyRoleStore = func(q store.Querier) policyRoleStore {
	return store.NewPolicyRoleStore(q)
}

// apiPolicyBoundary is the OpenAPI PolicyBoundary schema
// (openapi/v1.yaml #/components/schemas/PolicyBoundary): the declarative
// boundary an agent must stay within. It is free-form by design — the concrete
// rule schema is owned by the policy engine — so every field is optional and
// engine-specific rules ride in the open-ended rules map. It is the request
// shape (decoded from PolicyCreate/PolicyVersionCreate) and the canonical form
// that is marshaled into a version's content_json; responses echo the stored
// content_json back verbatim (see toAPIPolicyVersion).
type apiPolicyBoundary struct {
	AllowedTools  []string       `json:"allowed_tools,omitempty"`
	Network       string         `json:"network,omitempty"`
	FanOutCeiling *int           `json:"fan_out_ceiling,omitempty"`
	Rules         map[string]any `json:"rules,omitempty"`
}

// apiSigningMetadata is the OpenAPI SigningMetadata schema
// (openapi/v1.yaml #/components/schemas/SigningMetadata): the detached-signature
// metadata a client verifies when it receives the policy bundle at session
// launch. algorithm, key_id, and signature are required; signed_at is optional.
type apiSigningMetadata struct {
	Algorithm string     `json:"algorithm"`
	KeyID     string     `json:"key_id"`
	Signature string     `json:"signature"`
	SignedAt  *time.Time `json:"signed_at,omitempty"`
}

// apiPolicy is the OpenAPI Policy response schema
// (openapi/v1.yaml #/components/schemas/Policy). current_version maps from
// store.Policy.CurrentVersion and a nil description is omitted, mirroring the
// agent mapping (ADR-0002 §4a).
type apiPolicy struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    *string   `json:"description,omitempty"`
	CurrentVersion int       `json:"current_version"`
	CreatedAt      time.Time `json:"created_at"`
}

// apiPolicyList is the OpenAPI PolicyList response (a Page envelope plus items).
// next_cursor is nullable: a string on a full page and null once the list is
// exhausted (ADR-0002 §3), so it is a *string emitted without omitempty.
type apiPolicyList struct {
	Items      []apiPolicy `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// apiPolicyCreate is the OpenAPI PolicyCreate request body. name and boundary
// are required; description is optional. boundary is a pointer so an omitted
// boundary (a null pointer) is distinguishable from an empty-but-present
// boundary ({}), which is a valid free-form boundary.
type apiPolicyCreate struct {
	Name        string             `json:"name"`
	Description *string            `json:"description"`
	Boundary    *apiPolicyBoundary `json:"boundary"`
}

// apiPolicyVersion is the OpenAPI PolicyVersion response schema
// (openapi/v1.yaml #/components/schemas/PolicyVersion). digest maps from
// content_hash, boundary is the stored content_json echoed back verbatim, and
// signing is the stored signing_json — null for the unsigned version 1 created
// alongside a policy, an object for every published version.
type apiPolicyVersion struct {
	PolicyID  string          `json:"policy_id"`
	Version   int             `json:"version"`
	Digest    string          `json:"digest"`
	Boundary  json.RawMessage `json:"boundary"`
	Signing   json.RawMessage `json:"signing"`
	CreatedAt time.Time       `json:"created_at"`
}

// apiPolicyVersionCreate is the OpenAPI PolicyVersionCreate request body: both
// boundary and signing are required (an explicitly published version is always
// signed). Both are pointers so an omitted field is rejected as missing rather
// than silently defaulting.
type apiPolicyVersionCreate struct {
	Boundary *apiPolicyBoundary  `json:"boundary"`
	Signing  *apiSigningMetadata `json:"signing"`
}

// apiPolicyVersionList is the OpenAPI PolicyVersionList response. Unlike the
// paginated lists it is a plain {items} envelope (no Page/next_cursor): a
// policy's version history is bounded and returned whole.
type apiPolicyVersionList struct {
	Items []apiPolicyVersion `json:"items"`
}

// apiPolicyRoles is the OpenAPI PolicyRoles schema
// (openapi/v1.yaml #/components/schemas/PolicyRoles): the set of role ids a
// policy permits under the policy-centric access model (ADR-0010). It is both
// the PUT request body and the GET/PUT response. role_ids is emitted without
// omitempty and initialised to a non-nil slice so an empty permitted set
// serialises as [] rather than null.
type apiPolicyRoles struct {
	RoleIDs []string `json:"role_ids"`
}

// toAPIPolicy maps a store.Policy to its API representation.
func toAPIPolicy(p store.Policy) apiPolicy {
	return apiPolicy{
		ID:             p.ID,
		Name:           p.Name,
		Description:    p.Description,
		CurrentVersion: p.CurrentVersion,
		CreatedAt:      p.CreatedAt,
	}
}

// toAPIPolicyVersion maps a store.PolicyVersion to its API representation
// (digest ← content_hash, boundary ← content_json, signing ← signing_json). The
// stored jsonb columns are echoed back verbatim as json.RawMessage rather than
// re-decoded, so a version reads back byte-identical to what was hashed. A nil
// signing_json (the unsigned version 1) renders as JSON null.
func toAPIPolicyVersion(v store.PolicyVersion) apiPolicyVersion {
	return apiPolicyVersion{
		PolicyID:  v.PolicyID,
		Version:   v.Version,
		Digest:    v.ContentHash,
		Boundary:  json.RawMessage(v.ContentJSON),
		Signing:   json.RawMessage(v.SigningJSON),
		CreatedAt: v.CreatedAt,
	}
}

// canonicalBoundary marshals a boundary to its canonical JSON and derives the
// content hash stored alongside it. Marshaling a struct is deterministic (fixed
// field order, sorted map keys, no incidental whitespace), so two equal
// boundaries always produce the same bytes and therefore the same digest. The
// digest is "sha256:" + hex(sha256(canonical bytes)), the format the policy
// bundle carries at session launch.
func canonicalBoundary(b apiPolicyBoundary) (json.RawMessage, string, error) {
	content, err := json.Marshal(b)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(content)
	return content, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// listPolicies implements GET /policies (operationId listPolicies). It returns a
// keyset-paginated page of the calling organization's policies ordered by
// (created_at, id), mirroring listAgents (ADR-0002 §1 org from the token, §3
// pagination).
func (d Deps) listPolicies(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	limit, err := ParseLimit(r)
	if err != nil {
		WriteValidationError(w, "limit", "limit must be an integer")
		return
	}
	cursor, err := DecodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		WriteValidationError(w, "cursor", "invalid cursor")
		return
	}

	var afterCreatedAt time.Time
	var afterID string
	if cursor != nil {
		afterCreatedAt = cursor.CreatedAt
		afterID = cursor.ID
	}

	s := newPolicyStore(d.Pool)
	policies, err := s.ListByOrgPage(r.Context(), claims.OrgID, limit+1, afterCreatedAt, afterID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	var next *string
	if len(policies) > limit {
		policies = policies[:limit]
		c := EncodeCursor(Cursor{CreatedAt: policies[limit-1].CreatedAt, ID: policies[limit-1].ID})
		next = &c
	}

	items := make([]apiPolicy, 0, len(policies))
	for _, p := range policies {
		items = append(items, toAPIPolicy(p))
	}
	WriteOK(w, apiPolicyList{Items: items, NextCursor: next})
}

// createPolicy implements POST /policies (operationId createPolicy). It creates
// the policy row (org from the token, never the body) and, atomically from the
// caller's view, its first version: version 1 pins the supplied boundary
// unsigned (signing_json null — a version is signed only when explicitly
// published), and the policy's current_version is advanced to 1. A duplicate
// (org_id, name) surfaces as store.ErrConflict → 409.
func (d Deps) createPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	var req apiPolicyCreate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		WriteValidationError(w, "name", "name is required")
		return
	}
	if req.Boundary == nil {
		WriteValidationError(w, "boundary", "boundary is required")
		return
	}

	content, hash, err := canonicalBoundary(*req.Boundary)
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	ps := newPolicyStore(d.Pool)
	policy, err := ps.Create(ctx, claims.OrgID, req.Name, req.Description)
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	if _, err := newPolicyVersionStore(d.Pool).Create(ctx, store.CreatePolicyVersionParams{
		PolicyID:    policy.ID,
		Version:     1,
		ContentJSON: content,
		ContentHash: hash,
		SigningJSON: nil,
	}); err != nil {
		WriteStoreError(w, err)
		return
	}

	updated, err := ps.SetCurrentVersion(ctx, policy.ID, 1)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteCreated(w, toAPIPolicy(updated))
}

// getPolicy implements GET /policies/{policyId} (operationId getPolicy). The
// by-id lookup is not org-scoped, so after loading the row the handler enforces
// tenant isolation with SameOrg and returns 404 (not 403) on a cross-org id,
// per ADR-0002 §1.
func (d Deps) getPolicy(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())
	id := chi.URLParam(r, "policyId")

	policy, err := newPolicyStore(d.Pool).GetByID(r.Context(), id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, policy.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}
	WriteOK(w, toAPIPolicy(policy))
}

// listPolicyVersions implements GET /policies/{policyId}/versions (operationId
// listPolicyVersions). It confirms the policy belongs to the caller's org (a
// cross-org or missing policy is a 404, never 403) and returns the policy's
// immutable version history whole, ordered by version ascending.
func (d Deps) listPolicyVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	policyID := chi.URLParam(r, "policyId")

	policy, err := newPolicyStore(d.Pool).GetByID(ctx, policyID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, policy.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	versions, err := newPolicyVersionStore(d.Pool).ListByPolicy(ctx, policy.ID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	items := make([]apiPolicyVersion, 0, len(versions))
	for _, v := range versions {
		items = append(items, toAPIPolicyVersion(v))
	}
	WriteOK(w, apiPolicyVersionList{Items: items})
}

// publishPolicyVersion implements POST /policies/{policyId}/versions
// (operationId publishPolicyVersion). It appends a new signed version pinning
// the supplied boundary and its signing metadata, then advances the policy's
// current_version pointer to it. The next version number is derived from the
// policy's current_version (+1); a concurrent publish racing for the same number
// loses on the unique (policy_id, version) constraint and surfaces as a 409. A
// cross-org or missing policy is a 404.
func (d Deps) publishPolicyVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	policyID := chi.URLParam(r, "policyId")

	var req apiPolicyVersionCreate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.Boundary == nil {
		WriteValidationError(w, "boundary", "boundary is required")
		return
	}
	if req.Signing == nil {
		WriteValidationError(w, "signing", "signing is required")
		return
	}
	if req.Signing.Algorithm == "" {
		WriteValidationError(w, "signing.algorithm", "signing.algorithm is required")
		return
	}
	if req.Signing.KeyID == "" {
		WriteValidationError(w, "signing.key_id", "signing.key_id is required")
		return
	}
	if req.Signing.Signature == "" {
		WriteValidationError(w, "signing.signature", "signing.signature is required")
		return
	}

	ps := newPolicyStore(d.Pool)
	policy, err := ps.GetByID(ctx, policyID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, policy.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	content, hash, err := canonicalBoundary(*req.Boundary)
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}
	signing, err := json.Marshal(req.Signing)
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	next := policy.CurrentVersion + 1
	version, err := newPolicyVersionStore(d.Pool).Create(ctx, store.CreatePolicyVersionParams{
		PolicyID:    policy.ID,
		Version:     next,
		ContentJSON: content,
		ContentHash: hash,
		SigningJSON: signing,
	})
	if err != nil {
		WriteStoreError(w, err)
		return
	}

	if _, err := ps.SetCurrentVersion(ctx, policy.ID, next); err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteCreated(w, toAPIPolicyVersion(version))
}

// getPolicyRoles implements GET /policies/{policyId}/roles (operationId
// getPolicyRoles). It confirms the policy belongs to the caller's org (a
// cross-org or missing policy is a 404, never 403) and returns the ids of the
// roles the policy permits under the policy-centric access model (ADR-0010).
// The read is member-visible within the org; only the PUT is gated on
// policies.manage.
func (d Deps) getPolicyRoles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	policyID := chi.URLParam(r, "policyId")

	policy, err := newPolicyStore(d.Pool).GetByID(ctx, policyID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, policy.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	roleIDs, err := newPolicyRoleStore(d.Pool).ListRoles(ctx, policy.ID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteOK(w, apiPolicyRoles{RoleIDs: nonNilStrings(roleIDs)})
}

// updatePolicyRoles implements PUT /policies/{policyId}/roles (operationId
// updatePolicyRoles, permission policies.manage). It replaces the policy's
// permitted-role set wholesale (ADR-0010). Every supplied role id must name a
// role in the caller's org; an unknown or cross-org id is a 404 and nothing is
// written. On success it returns the stored set. A cross-org or missing policy
// is a 404.
func (d Deps) updatePolicyRoles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	policyID := chi.URLParam(r, "policyId")

	var req apiPolicyRoles
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	policy, err := newPolicyStore(d.Pool).GetByID(ctx, policyID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, policy.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	// Every role must exist and belong to the caller's org before we write:
	// validate up front so a bad id rejects the whole request rather than
	// leaving a partial set. A missing or cross-org role is a 404 (matching the
	// member-invite role check), never leaking another tenant's role ids.
	roles := newRoleStore(d.Pool)
	for _, roleID := range req.RoleIDs {
		role, err := roles.GetByID(ctx, roleID)
		if err != nil {
			WriteStoreError(w, err)
			return
		}
		if !SameOrg(claims.OrgID, role.OrgID) {
			WriteNotFound(w, "resource not found")
			return
		}
	}

	prs := newPolicyRoleStore(d.Pool)
	if err := prs.SetRoles(ctx, policy.ID, req.RoleIDs); err != nil {
		WriteStoreError(w, err)
		return
	}
	stored, err := prs.ListRoles(ctx, policy.ID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	WriteOK(w, apiPolicyRoles{RoleIDs: nonNilStrings(stored)})
}

// nonNilStrings returns s unchanged when non-nil, or an empty (non-nil) slice
// so a JSON array field serialises as [] rather than null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
