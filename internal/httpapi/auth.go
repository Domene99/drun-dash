package httpapi

import (
	"errors"
	"net/http"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Domene99/drun-dash/internal/auth"
	"github.com/Domene99/drun-dash/internal/authmw"
	"github.com/Domene99/drun-dash/internal/httpx"
	"github.com/Domene99/drun-dash/internal/store"
)

// oidcSessionExchangeRequest is the POST /auth/session body
// (openapi/v1.yaml #/components/schemas/OidcSessionExchangeRequest).
type oidcSessionExchangeRequest struct {
	// IDToken is the OIDC ID token the admin UI already obtained from the IdP.
	IDToken string `json:"id_token"`
	// OrganizationID scopes the (org_id, idp_subject) member lookup. It is
	// documented as optional in the contract, but a member cannot be resolved
	// without it, so this handler requires it.
	OrganizationID string `json:"organization_id"`
}

// tokenResponse is the POST /auth/session success body
// (openapi/v1.yaml #/components/schemas/TokenResponse).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// identityResponse is the GET /auth/whoami body
// (openapi/v1.yaml #/components/schemas/Identity).
type identityResponse struct {
	Subject        string   `json:"subject"`
	OrganizationID string   `json:"organization_id"`
	Email          string   `json:"email,omitempty"`
	Roles          []string `json:"roles"`
}

// exchangeOidcSession implements POST /auth/session (operationId
// exchangeOidcSession). It verifies the caller's OIDC ID token, maps the
// verified subject to a local member within the requested organization, and
// mints a short-lived control-plane access token for that member.
//
// A malformed body or a missing required field is a 400; every
// identity-resolution failure — an invalid/expired ID token, an unknown or
// cross-org subject, or an inactive member — is answered with a single generic
// 401 so the endpoint reveals nothing about which orgs or members exist.
//
// OIDC is optional at boot (cmd/control-plane wires Deps.OIDC only when the IdP
// is configured and reachable), so when it is nil this endpoint reports 501
// instead of the rest of the API being unavailable.
func (d Deps) exchangeOidcSession(w http.ResponseWriter, r *http.Request) {
	// OIDC is optional: when the IdP is unconfigured or was unreachable during
	// discovery at boot, Deps.OIDC is nil and this exchange cannot verify an ID
	// token. The rest of the API stays mounted (it needs only the access-token
	// Verifier), so the exchange degrades to 501 rather than panicking on a nil
	// d.OIDC.Verify. Guarded before decoding the body so the disabled state is
	// reported regardless of the request.
	if d.OIDC == nil {
		_ = httpx.WriteError(w, http.StatusNotImplemented, httpx.CodeNotImplemented,
			"OIDC session exchange is not configured on this server")
		return
	}

	var req oidcSessionExchangeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		_ = httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if req.IDToken == "" {
		WriteValidationError(w, "id_token", "id_token is required")
		return
	}
	if req.OrganizationID == "" {
		WriteValidationError(w, "organization_id", "organization_id is required")
		return
	}

	idToken, err := d.OIDC.Verify(r.Context(), req.IDToken)
	if err != nil {
		writeUnauthorized(w)
		return
	}

	member, err := d.Members.GetByIDPSubject(r.Context(), req.OrganizationID, idToken.Subject)
	if err != nil {
		// A missing (or cross-org) member is an authentication failure, not a
		// 404: returning 401 avoids leaking which subjects or orgs exist. Any
		// other error is a genuine server fault.
		if errors.Is(err, store.ErrNotFound) {
			writeUnauthorized(w)
			return
		}
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}
	if !member.Active {
		writeUnauthorized(w)
		return
	}

	// Subject mirrors members.idp_subject (per internal/auth), so /auth/whoami
	// and the request-auth middleware can resolve the member by (org, sub).
	// Mint fills iss/aud/iat/exp and a fresh jti; leaving RegisteredClaims.ID
	// empty lets it generate one.
	token, err := d.Issuer.Mint(r.Context(), auth.Claims{
		OrgID:            member.OrgID,
		MemberID:         member.ID,
		Roles:            []string{string(member.Role)},
		RegisteredClaims: jwt.RegisteredClaims{Subject: member.IDPSubject},
	})
	if err != nil {
		_ = httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		return
	}

	WriteOK(w, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int(d.AccessTTL.Seconds()),
	})
}

// getCurrentIdentity implements GET /auth/whoami (operationId
// getCurrentIdentity). It echoes the principal established by RequireAuth: the
// access token's subject, organization, and roles. It performs no lookups —
// email is not carried on the token, so it is omitted here.
func (d Deps) getCurrentIdentity(w http.ResponseWriter, r *http.Request) {
	claims := authmw.MustPrincipal(r.Context())

	// roles is a required array in the contract; coerce a nil claim to an
	// empty slice so it serialises as [] rather than null.
	roles := claims.Roles
	if roles == nil {
		roles = []string{}
	}

	WriteOK(w, identityResponse{
		Subject:        claims.Subject,
		OrganizationID: claims.OrgID,
		Roles:          roles,
	})
}

// writeUnauthorized writes the single generic 401 used for every
// authentication failure on the session-exchange path, matching the flat 401
// that authmw.RequireAuth emits (ADR-0002 §2). The body never discloses which
// check failed.
func writeUnauthorized(w http.ResponseWriter) {
	_ = httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
}
