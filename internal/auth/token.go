// Package auth is the session-token core for drun-dash: it mints and verifies
// the short-lived signed access JWT described in
// docs/adr/0001-authentication-and-session-model.md (§3, §5) and provides the
// Redis-backed revocation channel that makes those tokens promptly revocable.
//
// This package is a pure library. It has no HTTP routes, performs no OIDC/IdP
// exchange, and persists no refresh tokens — those live in later batches. It
// stops exactly where the ADR's proxy/middleware token check stops: "the token
// is valid and here are its claims." Resolving the Member from the database is
// a later middleware concern.
//
// # Tokens
//
// The access token is a short-lived JWT signed with an asymmetric key so the
// LLM proxy can verify it with only the public key and never holds signing
// material (ADR §3). Signing uses EdDSA (Ed25519); the header carries a kid so
// keys can rotate. An [Issuer] mints tokens from a private key; a [Verifier]
// checks them against a set of public keys keyed by kid.
//
// # Verification order (ADR §5)
//
// [Verifier.Verify] follows the ADR's order: parse the JWT, validate the
// signature and the iss/aud/exp claims against the cached public key selected
// by kid, then check the jti against the Redis revocation list. Because Redis
// is on the hot path, a revocation-check error is treated as fail-closed: the
// token is rejected, matching the threat model's "fail-closed offline" stance.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// signingAlg is the sole accepted JWT signing algorithm. The ADR mandates an
// asymmetric algorithm so the proxy verifies with a public key; EdDSA
// (Ed25519) is chosen for small keys and signatures. Pinning to exactly one
// algorithm and rejecting all others on verification defends against
// algorithm-confusion attacks.
const signingAlg = "EdDSA"

// Sentinel errors returned by [Verifier.Verify]. Callers can match them with
// errors.Is; parse/validation failures from the JWT library are wrapped and
// surfaced as-is so the reason (expired, bad signature, wrong iss/aud) is
// preserved for logging without being leaked to end users.
var (
	// ErrRevoked is returned when a token's jti is present on the Redis
	// revocation denylist.
	ErrRevoked = errors.New("auth: token revoked")

	// ErrMissingJTI is returned when a token carries no jti; without one it
	// cannot be checked against — or added to — the revocation list.
	ErrMissingJTI = errors.New("auth: token missing jti")
)

// Claims is the drun-dash access-token claim set (ADR §3). It embeds
// [jwt.RegisteredClaims] for the standard fields — iss, sub, aud, iat, nbf,
// exp, and jti (the RegisteredClaims.ID) — and adds the drun-dash-specific
// org, member, and role claims.
//
// sub deliberately mirrors members.idp_subject so that /auth/whoami and the
// request-authentication middleware can resolve the Member by (org_id, sub).
// The kid used to select a verification key lives in the JWT header, not here.
type Claims struct {
	// OrgID is the organization the member belongs to (members.org_id).
	OrgID string `json:"org_id"`
	// MemberID is the members.id primary key, carried so downstream code has
	// the local principal id without a lookup. Optional.
	MemberID string `json:"mid,omitempty"`
	// Roles are the coarse authorization roles granted to the member
	// (member_role: admin, member, auditor).
	Roles []string `json:"roles"`

	jwt.RegisteredClaims
}

// Issuer mints signed access tokens. Its private key, kid, issuer, audience,
// and TTL are supplied by the caller through [NewIssuer]; this package never
// reads them from config (the private key lives only in the control plane).
type Issuer struct {
	signingKey ed25519.PrivateKey
	kid        string
	issuer     string
	audience   string
	ttl        time.Duration

	now    func() time.Time
	newJTI func() (string, error)
}

// IssuerConfig configures a [NewIssuer]. SigningKey, KID, Issuer, Audience, and
// TTL are required; Now and NewJTI are optional seams for testing.
type IssuerConfig struct {
	// SigningKey is the Ed25519 private key the control plane signs with.
	SigningKey ed25519.PrivateKey
	// KID identifies SigningKey; it is written to the JWT header so a
	// [Verifier] can select the matching public key during rotation.
	KID string
	// Issuer is the token's iss claim (the control plane).
	Issuer string
	// Audience is the token's aud claim; distinct audiences for the
	// control-plane API and the LLM proxy stop a token minted for one from
	// being replayed at the other (ADR §3).
	Audience string
	// TTL is the access token's lifetime; the ADR recommends a short 15
	// minutes so the revocation denylist stays small.
	TTL time.Duration

	// Now overrides the clock (defaults to time.Now); for tests.
	Now func() time.Time
	// NewJTI overrides jti generation (defaults to 128 bits of crypto/rand);
	// for tests.
	NewJTI func() (string, error)
}

// NewIssuer validates cfg and returns an [Issuer]. It errors if the signing key
// is not a valid Ed25519 private key, if kid/issuer/audience are empty, or if
// TTL is not positive.
func NewIssuer(cfg IssuerConfig) (*Issuer, error) {
	if len(cfg.SigningKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("auth: new issuer: signing key must be a %d-byte ed25519 private key, got %d", ed25519.PrivateKeySize, len(cfg.SigningKey))
	}
	if cfg.KID == "" {
		return nil, fmt.Errorf("auth: new issuer: empty kid")
	}
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("auth: new issuer: empty issuer")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("auth: new issuer: empty audience")
	}
	if cfg.TTL <= 0 {
		return nil, fmt.Errorf("auth: new issuer: ttl must be positive, got %s", cfg.TTL)
	}

	iss := &Issuer{
		signingKey: cfg.SigningKey,
		kid:        cfg.KID,
		issuer:     cfg.Issuer,
		audience:   cfg.Audience,
		ttl:        cfg.TTL,
		now:        cfg.Now,
		newJTI:     cfg.NewJTI,
	}
	if iss.now == nil {
		iss.now = time.Now
	}
	if iss.newJTI == nil {
		iss.newJTI = randomJTI
	}
	return iss, nil
}

// KID reports the key id this issuer stamps into minted tokens.
func (i *Issuer) KID() string { return i.kid }

// Mint signs and returns an access token for c. The caller supplies the
// identity claims (Subject, OrgID, and optionally MemberID/Roles); Mint fills
// in the issuer, audience, iat, nbf, exp (now + TTL), and — unless the caller
// pre-set one — a unique jti, then stamps the issuer's kid into the header.
//
// The returned string is a compact-serialized JWT ready for an
// Authorization: Bearer header.
func (i *Issuer) Mint(ctx context.Context, c Claims) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.Subject == "" {
		return "", fmt.Errorf("auth: mint: empty subject")
	}
	if c.OrgID == "" {
		return "", fmt.Errorf("auth: mint: empty org_id")
	}

	now := i.now()
	if c.ID == "" {
		jti, err := i.newJTI()
		if err != nil {
			return "", fmt.Errorf("auth: mint: generate jti: %w", err)
		}
		c.ID = jti
	}
	c.Issuer = i.issuer
	c.Audience = jwt.ClaimStrings{i.audience}
	c.IssuedAt = jwt.NewNumericDate(now)
	c.NotBefore = jwt.NewNumericDate(now)
	c.ExpiresAt = jwt.NewNumericDate(now.Add(i.ttl))

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, &c)
	tok.Header["kid"] = i.kid

	signed, err := tok.SignedString(i.signingKey)
	if err != nil {
		return "", fmt.Errorf("auth: mint: sign: %w", err)
	}
	return signed, nil
}

// Verifier validates access tokens against a set of public keys keyed by kid
// and consults a [Revoker] for revocation. It is safe for concurrent use once
// constructed; the key set is read-only.
type Verifier struct {
	keys     map[string]ed25519.PublicKey
	issuer   string
	audience string
	revoker  Revoker
	leeway   time.Duration

	now func() time.Time
}

// VerifierConfig configures a [NewVerifier]. Keys, Issuer, Audience, and
// Revoker are required; Leeway and Now are optional.
type VerifierConfig struct {
	// Keys maps kid to the Ed25519 public key that verifies tokens signed
	// under that kid. Holding several keys at once permits zero-downtime
	// rotation.
	Keys map[string]ed25519.PublicKey
	// Issuer is the required iss claim.
	Issuer string
	// Audience is the required aud claim; it should match the audience this
	// verifier protects (the control-plane API or the proxy).
	Audience string
	// Revoker checks a token's jti against the revocation denylist. Required:
	// a stateless verifier could not honor the threat model's revocation
	// requirement.
	Revoker Revoker
	// Leeway is the allowed clock skew for exp/nbf checks; the ADR calls for a
	// small leeway. Defaults to 0.
	Leeway time.Duration

	// Now overrides the clock used for exp/nbf validation (defaults to
	// time.Now); for tests.
	Now func() time.Time
}

// NewVerifier validates cfg and returns a [Verifier]. It errors if the key set
// is empty or holds an invalid Ed25519 public key, if issuer/audience are
// empty, or if no revoker is supplied.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("auth: new verifier: at least one verification key required")
	}
	keys := make(map[string]ed25519.PublicKey, len(cfg.Keys))
	for kid, key := range cfg.Keys {
		if kid == "" {
			return nil, fmt.Errorf("auth: new verifier: empty kid in key set")
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("auth: new verifier: key %q must be a %d-byte ed25519 public key, got %d", kid, ed25519.PublicKeySize, len(key))
		}
		keys[kid] = key
	}
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("auth: new verifier: empty issuer")
	}
	if cfg.Audience == "" {
		return nil, fmt.Errorf("auth: new verifier: empty audience")
	}
	if cfg.Revoker == nil {
		return nil, fmt.Errorf("auth: new verifier: nil revoker")
	}

	v := &Verifier{
		keys:     keys,
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		revoker:  cfg.Revoker,
		leeway:   cfg.Leeway,
		now:      cfg.Now,
	}
	if v.now == nil {
		v.now = time.Now
	}
	return v, nil
}

// Verify parses and validates a compact-serialized access token and returns its
// claims. It performs, in order (ADR §5): parse the JWT and verify the EdDSA
// signature with the public key selected by the header kid; validate iss, aud,
// and exp/nbf (with the configured leeway); require a non-empty jti; and check
// that jti against the revocation denylist.
//
// A revocation-check error is fail-closed — the token is rejected rather than
// admitted — so an unavailable Redis denies access. A revoked token returns
// [ErrRevoked]; a missing jti returns [ErrMissingJTI]; signature/claim failures
// are wrapped from the JWT library.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims, v.keyFunc,
		jwt.WithValidMethods([]string{signingAlg}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(v.leeway),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, fmt.Errorf("auth: verify: %w", err)
	}
	if claims.ID == "" {
		return nil, fmt.Errorf("auth: verify: %w", ErrMissingJTI)
	}

	revoked, err := v.revoker.IsRevoked(ctx, claims.ID)
	if err != nil {
		// Fail closed: if the revocation channel cannot be consulted, do not
		// admit the token.
		return nil, fmt.Errorf("auth: verify: revocation check: %w", err)
	}
	if revoked {
		return nil, ErrRevoked
	}
	return &claims, nil
}

// keyFunc selects the verification key by the token header's kid and refuses
// tokens whose signing method is not EdDSA. WithValidMethods already pins the
// algorithm; this is defense in depth.
func (v *Verifier) keyFunc(t *jwt.Token) (any, error) {
	if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
		return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
	}
	kid, ok := t.Header["kid"].(string)
	if !ok || kid == "" {
		return nil, errors.New("missing kid header")
	}
	key, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return key, nil
}

// randomJTI returns 128 bits of cryptographically random data as a
// URL-safe, unpadded base64 string — enough entropy for a collision-free jti.
func randomJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
