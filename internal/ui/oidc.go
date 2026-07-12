package ui

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC environment variables the admin UI reads to enable the
// authorization-code browser login. When the required ones are unset the UI
// falls back to the paste-a-token dev login (see server.go / README.md).
const (
	envOIDCIssuer       = "OIDC_ISSUER"
	envOIDCClientID     = "OIDC_CLIENT_ID"
	envOIDCClientSecret = "OIDC_CLIENT_SECRET"
	envAdminUIRedirect  = "ADMIN_UI_REDIRECT_URL"
	envDefaultOrgID     = "DEFAULT_ORGANIZATION_ID"
)

// Transient cookies carrying the CSRF state token and the PKCE code verifier
// across the IdP redirect. Both are HttpOnly, short-lived (oidcLoginTTL), and
// cleared as soon as the callback consumes them — they exist only for the
// round trip to the identity provider and back.
const (
	oidcStateCookie    = "dd_oidc_state"
	oidcVerifierCookie = "dd_oidc_verifier"
)

// oidcLoginTTL bounds how long a started login (its state + PKCE verifier) stays
// valid. A login that is not completed within the window simply fails state
// validation and the operator starts over.
const oidcLoginTTL = 10 * time.Minute

// OIDCConfig is the admin-ui OIDC configuration, read from the environment by
// [OIDCConfigFromEnv]. Issuer, ClientID, and RedirectURL are required to enable
// the browser flow; ClientSecret is required by most (confidential-client)
// IdPs; OrganizationID is forwarded to the control plane's /auth/session so it
// can scope the minted session to the right org.
type OIDCConfig struct {
	Issuer         string
	ClientID       string
	ClientSecret   string
	RedirectURL    string
	OrganizationID string
}

// OIDCConfigFromEnv reads the OIDC settings from the process environment. It
// never fails: a config with any required field empty simply reports
// Enabled() == false, and the server keeps the paste-token dev login.
func OIDCConfigFromEnv() OIDCConfig {
	return OIDCConfig{
		Issuer:         os.Getenv(envOIDCIssuer),
		ClientID:       os.Getenv(envOIDCClientID),
		ClientSecret:   os.Getenv(envOIDCClientSecret),
		RedirectURL:    os.Getenv(envAdminUIRedirect),
		OrganizationID: os.Getenv(envDefaultOrgID),
	}
}

// Enabled reports whether the mandatory settings for the OIDC browser flow are
// present. When it returns false the admin UI keeps the documented
// paste-a-token dev login so local development still works.
func (c OIDCConfig) Enabled() bool {
	return c.Issuer != "" && c.ClientID != "" && c.RedirectURL != ""
}

// OIDCAuthenticator drives the OIDC authorization-code flow for the admin UI:
// it builds the IdP authorize URL (with CSRF state and a PKCE S256 challenge)
// and exchanges the returned code for tokens, handing back the raw ID token.
// It does not verify the ID token — that is the control plane's job on
// /auth/session (see internal/oidc + internal/auth); the admin UI only
// completes the browser handshake and relays the token.
type OIDCAuthenticator struct {
	oauth2 *oauth2.Config
	// orgID is the organization forwarded to /auth/session so the control plane
	// scopes the session it mints; it comes from DEFAULT_ORGANIZATION_ID.
	orgID string
}

// NewOIDCAuthenticator builds an authenticator by performing OIDC discovery
// against cfg.Issuer (a network call to .well-known/openid-configuration) to
// learn the authorize and token endpoints. It errors if the config is
// incomplete or discovery fails, so the binary fails fast at boot rather than
// on the first login.
func NewOIDCAuthenticator(ctx context.Context, cfg OIDCConfig) (*OIDCAuthenticator, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("ui: oidc config incomplete: %s, %s and %s are required",
			envOIDCIssuer, envOIDCClientID, envAdminUIRedirect)
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("ui: oidc discovery against %q: %w", cfg.Issuer, err)
	}
	return &OIDCAuthenticator{
		oauth2: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		orgID: cfg.OrganizationID,
	}, nil
}

// StartLogin mints a fresh CSRF state token and PKCE verifier and returns the
// IdP authorize URL to redirect the browser to, alongside the state and
// verifier the caller must persist (in the transient cookies) to validate the
// callback. The authorize URL carries the state and the S256 code challenge.
func (a *OIDCAuthenticator) StartLogin() (authURL, state, verifier string, err error) {
	state, err = newOIDCState()
	if err != nil {
		return "", "", "", err
	}
	verifier = oauth2.GenerateVerifier()
	authURL = a.oauth2.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	return authURL, state, verifier, nil
}

// Exchange trades an authorization code (with the PKCE verifier that started
// the flow) for the token set and returns the raw OIDC ID token. It errors if
// the exchange fails or the response carries no id_token — the ID token is the
// credential the control plane verifies, so a token response without one is
// unusable.
func (a *OIDCAuthenticator) Exchange(ctx context.Context, code, verifier string) (string, error) {
	tok, err := a.oauth2.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", fmt.Errorf("ui: oidc code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return "", errors.New("ui: token response carried no id_token")
	}
	return rawID, nil
}

// newOIDCState returns a cryptographically random, URL-safe CSRF state token.
func newOIDCState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ui: generate oidc state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
