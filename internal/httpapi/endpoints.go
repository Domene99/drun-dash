package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
	"github.com/Domene99/drun-dash/internal/vault"
)

// modelEndpointStore is the narrow slice of *[store.ModelEndpointStore] the
// endpoint handlers use. Naming it as an interface lets the tests inject an
// in-memory fake in place of a live database while the production path runs the
// real SQL, matching the other resource-handler seams.
type modelEndpointStore interface {
	Create(ctx context.Context, p store.CreateModelEndpointParams) (store.ModelEndpoint, error)
	GetByID(ctx context.Context, id string) (store.ModelEndpoint, error)
	ListByOrg(ctx context.Context, orgID string) ([]store.ModelEndpoint, error)
	Update(ctx context.Context, p store.UpdateModelEndpointParams) (store.ModelEndpoint, error)
	SetActive(ctx context.Context, id string, active bool) (store.ModelEndpoint, error)
	Delete(ctx context.Context, id string) error
}

// newModelEndpointStore builds the [modelEndpointStore] a handler operates
// through. It is a package-level var so tests can swap in a fake without a
// database; production constructs the real store over the request's pool.
var newModelEndpointStore = func(q store.Querier) modelEndpointStore {
	return store.NewModelEndpointStore(q)
}

// secretVault is the write side of the vault the endpoint handlers need: seal a
// new provider key, reseal (rotate) an existing one in place, and delete the
// sealed row when its endpoint is removed. It is satisfied by [vault.DBVault]
// and injected via [Deps.Vault] (the master key is process-wide config, like
// the bundle-signing key), so the handlers never touch the raw key material or
// the master key directly. Seal/Reseal report [vault.ErrKeyNotConfigured] when
// DRUNDASH_VAULT_KEY is unset, which the handlers surface as a clear 400.
type secretVault interface {
	Seal(ctx context.Context, orgID, plaintext string) (secretRef string, err error)
	Reseal(ctx context.Context, secretRef, plaintext string) error
	Delete(ctx context.Context, secretRef string) error
}

// apiModelEndpoint is the ModelEndpoint response schema
// (openapi/v1.yaml #/components/schemas/ModelEndpoint). It deliberately omits
// both the raw key and the opaque secret_ref: a caller learns only whether a
// key is set via the boolean secret_set (true when secret_ref != "").
type apiModelEndpoint struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	BaseURL   string    `json:"base_url"`
	Active    bool      `json:"active"`
	SecretSet bool      `json:"secret_set"`
	CreatedAt time.Time `json:"created_at"`
}

// apiModelEndpointList is the ModelEndpointList response — a plain {items}
// envelope (an org's endpoint set is bounded and returned whole).
type apiModelEndpointList struct {
	Items []apiModelEndpoint `json:"items"`
}

// apiModelEndpointCreate is the EndpointCreate request body. api_key is
// optional: when non-empty it is sealed into the DB vault and the endpoint
// records the resulting secret_ref; when empty the endpoint is created with no
// key (secret_set=false) and a key can be supplied later via PATCH.
type apiModelEndpointCreate struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

// apiModelEndpointUpdate is the EndpointUpdate request body: every field is
// optional (PATCH). A nil field keeps the stored value; a non-empty api_key
// rotates the sealed key; active toggles the endpoint.
type apiModelEndpointUpdate struct {
	Name     *string `json:"name"`
	Provider *string `json:"provider"`
	BaseURL  *string `json:"base_url"`
	APIKey   *string `json:"api_key"`
	Active   *bool   `json:"active"`
}

// toAPIModelEndpoint maps a store.ModelEndpoint to its API representation. It
// never surfaces the secret_ref or the key; secret_set reports only whether a
// credential is configured.
func toAPIModelEndpoint(e store.ModelEndpoint) apiModelEndpoint {
	return apiModelEndpoint{
		ID:        e.ID,
		OrgID:     e.OrgID,
		Name:      e.Name,
		Provider:  e.Provider,
		BaseURL:   e.BaseURL,
		Active:    e.Active,
		SecretSet: e.SecretRef != "",
		CreatedAt: e.CreatedAt,
	}
}

// listModelEndpoints implements GET /model-endpoints. It returns the calling
// organization's endpoints (org from the token) ordered by name, each without
// its key. Reads are member-visible; the write routes require endpoints.manage.
func (d Deps) listModelEndpoints(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	eps, err := newModelEndpointStore(d.Pool).ListByOrg(r.Context(), claims.OrgID)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	items := make([]apiModelEndpoint, 0, len(eps))
	for _, e := range eps {
		items = append(items, toAPIModelEndpoint(e))
	}
	WriteOK(w, apiModelEndpointList{Items: items})
}

// getModelEndpoint implements GET /model-endpoints/{endpointId}. The by-id
// lookup is not org-scoped, so after loading the row the handler enforces
// tenant isolation with SameOrg and returns 404 (not 403) on a cross-org id.
func (d Deps) getModelEndpoint(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())
	id := chi.URLParam(r, "endpointId")

	ep, err := newModelEndpointStore(d.Pool).GetByID(r.Context(), id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, ep.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}
	WriteOK(w, toAPIModelEndpoint(ep))
}

// createModelEndpoint implements POST /model-endpoints (requires
// endpoints.manage). It registers an endpoint in the caller's org (org from the
// token, never the body). A non-empty api_key is sealed into the DB vault and
// its ref stored as secret_ref; an empty api_key stores no key. A duplicate
// (org_id, name) is a 409; an api_key supplied while DRUNDASH_VAULT_KEY is unset
// is a 400 telling the operator to set it. The response never contains the key.
func (d Deps) createModelEndpoint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)

	var req apiModelEndpointCreate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if !validateEndpointFields(w, req.Name, req.Provider, req.BaseURL) {
		return
	}

	// Seal the key (if any) before the insert, so a vault failure never leaves a
	// keyless endpoint behind. An empty key means "no key yet".
	secretRef := ""
	if req.APIKey != "" {
		ref, ok := d.sealKey(w, ctx, claims.OrgID, req.APIKey)
		if !ok {
			return
		}
		secretRef = ref
	}

	ep, err := newModelEndpointStore(d.Pool).Create(ctx, store.CreateModelEndpointParams{
		OrgID:     claims.OrgID,
		Name:      req.Name,
		BaseURL:   req.BaseURL,
		Provider:  req.Provider,
		SecretRef: secretRef,
	})
	if err != nil {
		// The endpoint insert failed after the key was sealed; drop the orphan
		// secret so a retried create does not leak rows. Best-effort: a cleanup
		// failure is logged by the vault layer, not surfaced.
		if secretRef != "" && vault.IsDBRef(secretRef) {
			_ = d.Vault.Delete(ctx, secretRef)
		}
		WriteStoreError(w, err)
		return
	}
	WriteCreated(w, toAPIModelEndpoint(ep))
}

// updateModelEndpoint implements PATCH /model-endpoints/{endpointId} (requires
// endpoints.manage). It loads the endpoint, enforces tenant isolation
// (cross-org or missing → 404), merges the supplied fields, rotates the sealed
// key when a non-empty api_key is given (reseal in place if a db-backed secret
// already exists, else seal a new one), applies the metadata update, and toggles
// active when supplied. The response never contains the key.
func (d Deps) updateModelEndpoint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "endpointId")

	var req apiModelEndpointUpdate
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	s := newModelEndpointStore(d.Pool)
	ep, err := s.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, ep.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	name := ep.Name
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			WriteValidationError(w, "name", "name must not be empty")
			return
		}
		name = *req.Name
	}
	provider := ep.Provider
	if req.Provider != nil {
		if strings.TrimSpace(*req.Provider) == "" {
			WriteValidationError(w, "provider", "provider must not be empty")
			return
		}
		provider = *req.Provider
	}
	baseURL := ep.BaseURL
	if req.BaseURL != nil {
		if strings.TrimSpace(*req.BaseURL) == "" {
			WriteValidationError(w, "base_url", "base_url must not be empty")
			return
		}
		baseURL = *req.BaseURL
	}

	// Rotate the key when a non-empty api_key is supplied: reseal an existing
	// db-backed secret in place (keeping the same secret_ref), otherwise seal a
	// fresh one. An empty/omitted api_key leaves the stored key untouched.
	secretRef := ep.SecretRef
	if req.APIKey != nil && *req.APIKey != "" {
		if vault.IsDBRef(ep.SecretRef) {
			if err := d.Vault.Reseal(ctx, ep.SecretRef, *req.APIKey); err != nil {
				d.writeVaultError(w, err)
				return
			}
		} else {
			ref, ok := d.sealKey(w, ctx, ep.OrgID, *req.APIKey)
			if !ok {
				return
			}
			secretRef = ref
		}
	}

	updated, err := s.Update(ctx, store.UpdateModelEndpointParams{
		ID:        ep.ID,
		Name:      name,
		BaseURL:   baseURL,
		Provider:  provider,
		SecretRef: secretRef,
	})
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if req.Active != nil {
		updated, err = s.SetActive(ctx, ep.ID, *req.Active)
		if err != nil {
			WriteStoreError(w, err)
			return
		}
	}
	WriteOK(w, toAPIModelEndpoint(updated))
}

// deleteModelEndpoint implements DELETE /model-endpoints/{endpointId} (requires
// endpoints.manage). It loads the endpoint for the tenant-isolation check
// (cross-org or missing → 404), deletes it, and then removes the backing sealed
// secret when the ref is db-backed. The secret cleanup is best-effort — the
// endpoint is already gone — so a missing secret row is not an error.
func (d Deps) deleteModelEndpoint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := authmw.MustPrincipal(ctx)
	id := chi.URLParam(r, "endpointId")

	s := newModelEndpointStore(d.Pool)
	ep, err := s.GetByID(ctx, id)
	if err != nil {
		WriteStoreError(w, err)
		return
	}
	if !SameOrg(claims.OrgID, ep.OrgID) {
		WriteNotFound(w, "resource not found")
		return
	}

	if err := s.Delete(ctx, ep.ID); err != nil {
		WriteStoreError(w, err)
		return
	}
	if vault.IsDBRef(ep.SecretRef) {
		if err := d.Vault.Delete(ctx, ep.SecretRef); err != nil && !errors.Is(err, vault.ErrNotFound) {
			// The endpoint is deleted; a stray secret row is a cleanup concern,
			// not a client-facing failure. Log via the standard path and still
			// return 204.
			slog.WarnContext(ctx, "delete backing secret failed after endpoint delete",
				slog.String("endpoint_id", ep.ID), slog.Any("error", err))
		}
	}
	WriteNoContent(w)
}

// sealKey seals plaintext for orgID and returns the resulting secret_ref. On a
// vault failure it writes the response (400 when the master key is unset, 500
// otherwise) and returns ok=false; the caller must stop.
func (d Deps) sealKey(w http.ResponseWriter, ctx context.Context, orgID, plaintext string) (string, bool) {
	ref, err := d.Vault.Seal(ctx, orgID, plaintext)
	if err != nil {
		d.writeVaultError(w, err)
		return "", false
	}
	return ref, true
}

// writeVaultError maps a vault seal/reseal failure to a response: a missing
// master key is a 400 with an operator-actionable message; anything else is a
// generic 500 that never echoes secret material.
func (d Deps) writeVaultError(w http.ResponseWriter, err error) {
	if errors.Is(err, vault.ErrKeyNotConfigured) {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest,
			"cannot store an api_key: set DRUNDASH_VAULT_KEY (base64-encoded 32-byte key) on the control plane")
		return
	}
	writeInternalError(w)
}

// validateEndpointFields enforces the required-field rules shared by create.
// It writes the 400 and returns false on the first violation.
func validateEndpointFields(w http.ResponseWriter, name, provider, baseURL string) bool {
	switch {
	case strings.TrimSpace(name) == "":
		WriteValidationError(w, "name", "name is required")
		return false
	case strings.TrimSpace(provider) == "":
		WriteValidationError(w, "provider", "provider is required")
		return false
	case strings.TrimSpace(baseURL) == "":
		WriteValidationError(w, "base_url", "base_url is required")
		return false
	}
	return true
}
