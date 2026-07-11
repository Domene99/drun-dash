# internal/oidc

Verifies the **OIDC ID token** issued by an organization's identity provider.
It implements the IdP-verification half of §1 of
[`docs/adr/0001-authentication-and-session-model.md`](../../docs/adr/0001-authentication-and-session-model.md):
given a raw ID token, check its **signature against the issuer's JWKS** and
validate the standard `iss` / `aud` / `exp` claims, returning the verified
identity — notably the `sub` used to look up the local member.

This package is a **pure library**. It has no HTTP routes, does not run the OIDC
authorization-code flow, and — importantly — **does not mint the drun-dash
session JWT**. Session-token minting and verification live in
[`internal/auth`](../auth); that package takes the identity returned here and
issues its own short-lived access token. This package is strictly the *upstream
IdP* side.

## Claim → member mapping

The verified `sub` claim maps to **`members.idp_subject`**. The session-exchange
endpoint (a later batch) resolves the local `Member` by `(org_id, sub)` — the
`members` table has `UNIQUE (org_id, idp_subject)` — and only then feeds the
identity into `auth.Issuer` to mint a session token. `email` is lifted out for
convenience; the full decoded claim set is returned as `Claims` so callers can
read IdP-specific fields (name, groups, …) without this package enumerating
them.

## Verifiers

```go
type Verifier interface {
    Verify(ctx context.Context, rawIDToken string) (*IDToken, error)
}

type IDToken struct {
    Subject  string         // sub → members.idp_subject (always set on success)
    Email    string         // email claim, or "" if absent
    Issuer   string         // verified iss
    Audience []string       // verified aud (contains the org's client ID)
    Expiry   time.Time      // exp
    IssuedAt time.Time      // iat
    Claims   map[string]any // the full decoded claim set, verbatim
}
```

`Verify` validates the signature and the `iss`, `aud` (the org's client ID), and
`exp` claims, then requires a non-empty `sub`; it returns a clear error on each
failure mode and `ErrMissingSubject` when the token has no `sub`.

Two constructors build a `Verifier`; they differ only in **where the signing
keys come from**:

### `NewRemoteVerifier` — production

```go
v, err := oidc.NewRemoteVerifier(ctx, issuerURL, clientID)
```

Performs live **OIDC discovery** against `issuerURL`
(`/.well-known/openid-configuration`) and verifies against the issuer's JWKS.
Key fetching, caching, and `kid`-miss re-fetch (to follow key rotation) are
handled by [`github.com/coreos/go-oidc/v3`](https://github.com/coreos/go-oidc).
`issuerURL` is the org's OIDC issuer and `clientID` is the org's registered
OAuth client ID (checked as `aud`).

### `NewStaticVerifier` — tests & static configs

```go
v, err := oidc.NewStaticVerifier(oidc.StaticConfig{
    Issuer:     "https://idp.example.com",
    ClientID:   "drun-dash-org-client",
    PublicKeys: []crypto.PublicKey{pub}, // or KeySet: aParsedJWKS
})
```

Takes **injected** verification material — a slice of `crypto.PublicKey` or an
`oidc.KeySet` — and does **no network I/O**. This is the seam that keeps the
package testable offline (mirroring how `internal/auth.Verifier` accepts its
public keys directly) and lets a deployment pin a static JWKS. `StaticConfig`
also exposes `Now` and `Skip{ClientID,Expiry,Issuer}Check` for tests and
statically-validated setups.

## Tests

`verifier_test.go` generates a local RSA key, signs ID tokens with the existing
`golang-jwt/jwt/v5` library, and verifies them through `NewStaticVerifier`. It
covers the happy path, expired token, wrong audience, wrong issuer, bad
signature, and missing `sub`, plus constructor validation. Everything runs with
**no network** — no live IdP, discovery, or JWKS fetch — so the suite runs in
the default `go test ./...`.

## Not in this package (elsewhere / later batches)

Minting and verifying the drun-dash **session JWT** ([`internal/auth`](../auth)),
the OIDC authorization-code flow and the `POST /auth/session` HTTP handler, the
`members` lookup that maps `sub` → `Member`, and per-org issuer/client-ID
configuration.
