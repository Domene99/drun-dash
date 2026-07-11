// Package oidc verifies OIDC ID tokens issued by an organization's identity
// provider. Per docs/adr/0001-authentication-and-session-model.md (§1), the
// control plane authenticates a member on POST /auth/session by verifying the
// IdP-issued ID token — signature against the issuer's JWKS, plus the standard
// iss/aud/exp claims — and then maps the verified sub claim to
// members.idp_subject. This package performs only that upstream verification.
//
// # Boundaries
//
// This is a pure library. It does not mint or verify the drun-dash session
// JWT — that is internal/auth, which takes the identity returned here and mints
// its own short-lived access token. It exposes no HTTP routes and completes the
// OIDC authorization-code flow nowhere; it starts from a raw ID token and ends
// at "the token is valid and here are its verified claims." Resolving the
// Member from Postgres by (org_id, sub) is a later middleware concern.
//
// # Verifiers
//
// A [Verifier] validates a compact-serialized ID token. Two constructors build
// one:
//
//   - [NewRemoteVerifier] performs live OIDC discovery against an issuer URL
//     (fetching /.well-known/openid-configuration and the JWKS, with caching
//     and kid-miss re-fetch handled by go-oidc). Use it in production.
//   - [NewStaticVerifier] takes pre-supplied verification material (a key set
//     or a slice of public keys) so the package is fully testable offline and
//     usable with statically configured keys. It performs no network I/O.
//
// The key source is injected exactly so tests never hit the network, mirroring
// how internal/auth.Verifier accepts its public keys directly.
package oidc

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrMissingSubject is returned by [Verifier.Verify] when a token passes
// signature and claim validation but carries no sub claim. The sub is the one
// claim this package exists to produce — it maps to members.idp_subject — so a
// token without it is unusable even though it is otherwise well-formed.
var ErrMissingSubject = errors.New("oidc: id token missing sub claim")

// IDToken is the verified identity extracted from an OIDC ID token. Subject and
// Email are lifted out for convenience; Claims holds the full decoded claim set
// so callers can read IdP-specific fields (groups, name, ...) without this
// package enumerating them.
type IDToken struct {
	// Subject is the sub claim — the IdP's stable identifier for the user. It
	// maps to members.idp_subject; the session-exchange endpoint resolves the
	// local Member by (org_id, Subject). Always non-empty on a successful
	// Verify.
	Subject string
	// Email is the email claim, or "" if the IdP did not include one. It is a
	// convenience copy of Claims["email"]; do not treat it as verified contact
	// information unless the IdP also asserts email_verified.
	Email string
	// Issuer is the verified iss claim.
	Issuer string
	// Audience is the verified aud claim(s); it contains the org's registered
	// client ID.
	Audience []string
	// Expiry is the exp claim.
	Expiry time.Time
	// IssuedAt is the iat claim.
	IssuedAt time.Time
	// Claims is the full decoded claim set, exactly as the IdP sent it.
	Claims map[string]any
}

// Verifier validates OIDC ID tokens. Implementations are safe for concurrent
// use.
type Verifier interface {
	// Verify parses rawIDToken, checks its signature against the configured key
	// source, validates iss/aud/exp, and returns the verified identity. It
	// returns a non-nil error on any validation failure and [ErrMissingSubject]
	// when the token carries no sub.
	Verify(ctx context.Context, rawIDToken string) (*IDToken, error)
}

// verifier adapts *oidc.IDTokenVerifier to the [Verifier] interface. Both
// constructors produce one of these; only the underlying key source differs
// (a remote JWKS for [NewRemoteVerifier], injected keys for
// [NewStaticVerifier]).
type verifier struct {
	inner *oidc.IDTokenVerifier
}

// NewRemoteVerifier builds a [Verifier] that verifies tokens against the live
// IdP at issuerURL. It performs OIDC discovery now (a network call) and returns
// an error if discovery fails; the JWKS itself is fetched lazily and cached by
// go-oidc, which re-fetches on a kid miss to follow key rotation.
//
// issuerURL is the org's OIDC issuer (the iss the tokens will carry); clientID
// is the org's registered OAuth client ID, checked as the aud claim. Both are
// required.
func NewRemoteVerifier(ctx context.Context, issuerURL, clientID string) (Verifier, error) {
	if issuerURL == "" {
		return nil, fmt.Errorf("oidc: new remote verifier: empty issuer url")
	}
	if clientID == "" {
		return nil, fmt.Errorf("oidc: new remote verifier: empty client id")
	}
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: new remote verifier: discovery: %w", err)
	}
	return &verifier{inner: provider.Verifier(&oidc.Config{ClientID: clientID})}, nil
}

// StaticConfig configures a [NewStaticVerifier]. It supplies the verification
// material directly instead of fetching it, so the verifier does no network I/O
// — the seam that makes this package testable offline and usable with a
// statically pinned JWKS.
type StaticConfig struct {
	// Issuer is the expected iss claim. Required unless SkipIssuerCheck.
	Issuer string
	// ClientID is the expected aud claim (the org's client ID). Required unless
	// SkipClientIDCheck.
	ClientID string

	// PublicKeys are the IdP signing public keys tokens are verified against.
	// Supply either PublicKeys or KeySet (not both required; PublicKeys is the
	// simple path). Any crypto.PublicKey the signing algorithm accepts works
	// (*rsa.PublicKey, ed25519.PublicKey, *ecdsa.PublicKey).
	PublicKeys []crypto.PublicKey
	// KeySet is an explicit oidc.KeySet, for callers that already hold one
	// (e.g. a parsed JWKS). Takes precedence over PublicKeys when set.
	KeySet oidc.KeySet

	// SupportedSigningAlgs restricts the accepted JWS algorithms. Defaults to
	// go-oidc's default (RS256) when empty; set it to pin a different algorithm.
	SupportedSigningAlgs []string

	// Now overrides the clock used for exp/nbf validation (defaults to
	// time.Now); for tests.
	Now func() time.Time

	// SkipClientIDCheck disables the aud check. Use only for static setups that
	// validate the audience elsewhere.
	SkipClientIDCheck bool
	// SkipExpiryCheck disables the exp check. Intended for tests.
	SkipExpiryCheck bool
	// SkipIssuerCheck disables the iss check. Intended for tests.
	SkipIssuerCheck bool
}

// NewStaticVerifier builds a [Verifier] from injected verification material. It
// performs no network I/O. It errors if no key source is given, or if the
// issuer/client ID required by the enabled checks is empty.
func NewStaticVerifier(cfg StaticConfig) (Verifier, error) {
	keySet := cfg.KeySet
	if keySet == nil {
		if len(cfg.PublicKeys) == 0 {
			return nil, fmt.Errorf("oidc: new static verifier: no verification keys (set PublicKeys or KeySet)")
		}
		keySet = &oidc.StaticKeySet{PublicKeys: cfg.PublicKeys}
	}
	if !cfg.SkipIssuerCheck && cfg.Issuer == "" {
		return nil, fmt.Errorf("oidc: new static verifier: empty issuer (set Issuer or SkipIssuerCheck)")
	}
	if !cfg.SkipClientIDCheck && cfg.ClientID == "" {
		return nil, fmt.Errorf("oidc: new static verifier: empty client id (set ClientID or SkipClientIDCheck)")
	}

	inner := oidc.NewVerifier(cfg.Issuer, keySet, &oidc.Config{
		ClientID:             cfg.ClientID,
		SupportedSigningAlgs: cfg.SupportedSigningAlgs,
		SkipClientIDCheck:    cfg.SkipClientIDCheck,
		SkipExpiryCheck:      cfg.SkipExpiryCheck,
		SkipIssuerCheck:      cfg.SkipIssuerCheck,
		Now:                  cfg.Now,
	})
	return &verifier{inner: inner}, nil
}

// Verify implements [Verifier]. It delegates signature and iss/aud/exp
// validation to go-oidc, then decodes the full claim set and enforces the
// presence of sub.
func (v *verifier) Verify(ctx context.Context, rawIDToken string) (*IDToken, error) {
	tok, err := v.inner.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify: %w", err)
	}

	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: verify: decode claims: %w", err)
	}
	if tok.Subject == "" {
		return nil, ErrMissingSubject
	}

	email, _ := claims["email"].(string)
	return &IDToken{
		Subject:  tok.Subject,
		Email:    email,
		Issuer:   tok.Issuer,
		Audience: tok.Audience,
		Expiry:   tok.Expiry,
		IssuedAt: tok.IssuedAt,
		Claims:   claims,
	}, nil
}
