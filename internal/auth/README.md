# internal/auth

The **session-token core** for drun-dash: minting and verifying the short-lived
signed **access JWT**, and the **Redis-backed revocation** channel that makes
those tokens promptly revocable. It implements §3 and §5 of
[`docs/adr/0001-authentication-and-session-model.md`](../../docs/adr/0001-authentication-and-session-model.md).

This package is a **pure library**. It has no HTTP routes, does no OIDC/IdP
exchange, and persists no refresh tokens — those are later batches. It stops
exactly where the ADR's proxy/middleware token check stops: *"the token is valid
and here are its claims."* Resolving the `Member` from Postgres is a later
middleware concern.

## Tokens

The access token is a short-lived JWT signed with an **asymmetric** key
(**EdDSA / Ed25519**), so the LLM proxy verifies with only the *public* key and
never holds signing material (ADR §3). The header carries a `kid` so keys can
rotate.

- **`Issuer`** mints tokens from an Ed25519 private key. The caller supplies the
  identity claims; the issuer fills in `iss`, `aud`, `iat`, `nbf`, `exp`
  (`now + TTL`), a unique `jti`, and stamps its `kid` into the header.
- **`Verifier`** validates tokens against a set of public keys keyed by `kid`
  and consults a `Revoker`.

Signing keys are **supplied by the caller** through the constructors — this
package reads nothing from `internal/config` (the private key lives only in the
control plane).

### Claims (ADR §3)

`Claims` embeds `jwt.RegisteredClaims` (`iss`, `sub`, `aud`, `iat`, `nbf`,
`exp`, `jti`) and adds:

| Claim   | JSON      | Meaning                                                   |
| ------- | --------- | --------------------------------------------------------- |
| Subject | `sub`     | `members.idp_subject` — resolve the Member by `(org_id, sub)` |
| OrgID   | `org_id`  | `members.org_id`                                          |
| MemberID| `mid`     | `members.id` (optional)                                   |
| Roles   | `roles`   | coarse `member_role`s (`admin`, `member`, `auditor`)      |

The `kid` lives in the JWT **header**, not the claims.

## Verification order (ADR §5)

`Verifier.Verify` follows the ADR's order:

1. Parse the Bearer JWT and verify the **EdDSA signature** with the public key
   selected by the header `kid` (the signing method is pinned to EdDSA — any
   other `alg` is rejected).
2. Validate `iss`, `aud`, and `exp`/`nbf` (with a configurable clock-skew
   leeway; `exp` is required).
3. Require a non-empty `jti` and check it against the Redis **revocation
   denylist**.

A revocation-check error is **fail-closed** — the token is rejected, not
admitted — so an unavailable Redis denies access, matching the threat model's
"fail-closed offline" stance. `Verify` returns `ErrRevoked` for a denylisted
token and `ErrMissingJTI` for a token with no `jti`.

## Revocation

`Revoker` is the revocation channel:

```go
type Revoker interface {
    Revoke(ctx context.Context, jti string, ttl time.Duration) error
    IsRevoked(ctx context.Context, jti string) (bool, error)
}
```

`RedisRevoker` implements it over the shared [`internal/redis`](../redis)
`Client`. Each revoked `jti` is a Redis key (`authz:revoked:jti:<jti>` by
default) with a **TTL equal to the token's remaining lifetime**, so entries
self-expire and the denylist stays small (ADR §3). `Revoke` refuses a
non-positive TTL, which would otherwise create a key that never expires.

## Usage

```go
// Control plane — mint.
issuer, err := auth.NewIssuer(auth.IssuerConfig{
    SigningKey: privKey,          // ed25519.PrivateKey, from the secret store
    KID:        "control-2026-07",
    Issuer:     "https://control-plane.example",
    Audience:   "drun-dash-control-plane",
    TTL:        15 * time.Minute,
})
token, err := issuer.Mint(ctx, auth.Claims{
    OrgID:            "org-uuid",
    MemberID:         "member-uuid",
    Roles:            []string{"member"},
    RegisteredClaims: jwt.RegisteredClaims{Subject: idpSubject},
})

// Proxy / middleware — verify.
revoker, err := auth.NewRedisRevoker(redisClient)
verifier, err := auth.NewVerifier(auth.VerifierConfig{
    Keys:     map[string]ed25519.PublicKey{"control-2026-07": pubKey},
    Issuer:   "https://control-plane.example",
    Audience: "drun-dash-control-plane",
    Revoker:  revoker,
    Leeway:   30 * time.Second,
})
claims, err := verifier.Verify(ctx, token)

// Revoke a session (TTL = token's remaining lifetime).
err = revoker.Revoke(ctx, claims.ID, time.Until(claims.ExpiresAt.Time))
```

Distinct audiences for the control-plane API and the LLM proxy keep a token
minted for one from being replayed at the other; construct a `Verifier` per
audience.

## Tests

Unit tests (`token_test.go`) cover the mint→verify round trip, expiry, bad
signature, unknown `kid`, wrong `iss`/`aud`, revoked `jti`, missing `jti`, and
fail-closed behavior on a revoker error. They use an in-memory fake `Revoker`
and need **no Redis** — they run in the default `go test ./...`.

Integration tests (`revocation_test.go`) exercise `RedisRevoker` against a real
Redis 7 and are gated behind the `integration` build tag. They **skip** (not
fail) when the server is unreachable.

```sh
# Start the local Postgres 16 + Redis 7 stack.
docker compose -f deploy/docker/docker-compose.yml up -d

# Run the integration tests against it.
go test -tags integration ./internal/auth/...
```

Override the target server with `REDIS_URL` (default: `config.DefaultRedisURL`).

## Not in this package (later batches)

OIDC/IdP exchange and JWKS validation, the device-authorization flow, refresh
tokens (`auth_refresh_tokens` in Postgres), the chi authentication middleware
and `Member` resolution, signing-key generation/rotation and secret-store
integration. See the ADR's "out of scope" section.
