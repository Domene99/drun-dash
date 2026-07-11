# 0001 — Authentication and session model

- **Status:** Proposed
- **Date:** 2026-07-11
- **Deciders:** drun-dash control-plane / CLI work streams
- **Batch:** 5 (decision artifact; no code)

## Context

Authentication sits on the critical path for drun-dash. Several later work
streams touch it independently — the control-plane auth endpoints, the CLI
device-flow client, the request-authentication middleware, and the LLM proxy's
token check. If each picks its own approach the pieces will not compose. This
ADR **decides the approach once**, grounded in what already exists on `main`,
so those implementations stay consistent. It writes no code.

What already exists and constrains this decision:

- **API contract — `openapi/v1.yaml`.** The `Auth` surface is already defined:
  `POST /auth/device/code` (`startDeviceAuthorization`), `POST /auth/device/token`
  (`pollDeviceToken`), `POST /auth/session` (`exchangeOidcSession`), and
  `GET /auth/whoami` (`getCurrentIdentity`). The global security scheme is
  `bearerAuth` — an `http`/`bearer` scheme with `bearerFormat: JWT`, described
  as a "Control-plane Bearer JWT … obtained from the OAuth 2.0 device flow (CLI)
  or the OIDC session exchange (admin UI)." `TokenResponse` returns
  `access_token` + `token_type: Bearer` + `expires_in` + an optional
  `refresh_token`. The device endpoints already speak RFC 8628 error codes
  (`authorization_pending`, `slow_down`, `access_denied`, `expired_token`).
  **The contract has already committed to a signed JWT session token**; this ADR
  ratifies that and fills in the parts the contract leaves open.
- **Design constraints (already agreed).** SSO via the org IdP; **v1 is
  OIDC-only, SAML deferred**; the CLI uses the OAuth 2.0 device-authorization
  grant (RFC 8628); identity lives entirely in the client/control-plane layer —
  the drun engine has no concept of a user.
- **Schema — `migrations/000002_identity.up.sql`.** `members(id, org_id,
  idp_subject, email, display_name, role member_role, active, created_at)` with
  `UNIQUE (org_id, idp_subject)` is the local principal, keyed by the IdP
  `subject` claim. `member_role` is an enum (`admin`, `member`, `auditor`).
  There is **no human-session table** — the `sessions` table is for agent runs,
  not auth. This ADR decides where auth/session state lives.
- **Infrastructure.** PostgreSQL 16 and Redis 7 (`internal/redis` is on `main`,
  connectivity only) are both available; Redis is the intended home for
  short-lived device-code/session state.
- **CLI reality.** `internal/cli/login.go` carries a `TODO(batch/5)` describing
  the intended device flow; `internal/cli/whoami.go` is a stub. Neither stores
  a token nor makes network calls yet.
- **Threat model — `docs/security/threat-model.md`.** The laptop is
  *semi-trusted* and holds a **short-lived, scoped session token, never a master
  key**. "Tokens are short-lived and per-session; revocation at the control
  plane cuts off … access even if the token has not yet expired." Prompt
  revocation is therefore a hard requirement, not a nicety.

## Decision drivers

- **Contract fidelity.** Stay aligned with `openapi/v1.yaml`; propose contract
  changes explicitly, with rationale, only where genuinely needed.
- **Prompt revocation.** The threat model requires that a stolen token can be
  cut off before it expires.
- **Proxy can validate cheaply.** The LLM proxy is on the hot path and must
  check the token without a database round-trip per request where possible.
- **One language, few dependencies.** Go 1.22+, small maintained libraries,
  standard `net/http` + chi middleware (per `TECH_STACK.md`).
- **No master credentials on the developer machine.** The CLI's stored token is
  a *user session* token, not an LLM provider key.

## Decision

### 1. IdP integration (OIDC, v1)

- **Discovery & validation.** Each organization is configured with an OIDC
  issuer URL. The control plane fetches the issuer's
  `/.well-known/openid-configuration` and its JWKS, caches the signing keys
  (respecting cache headers, with periodic refresh and `kid`-miss re-fetch), and
  validates the IdP-issued **ID token** on `POST /auth/session`: signature
  against the JWKS, plus `iss`, `aud` (the org's registered client ID), `exp`,
  `nbf`, and `nonce` checks. Only the admin UI calls this endpoint, server-side,
  after it completes the OIDC authorization-code flow with the IdP.
- **Library.** Use **`github.com/coreos/go-oidc/v3`** for discovery and ID-token
  verification, layered on **`golang.org/x/oauth2`** for the authorization-code
  exchange. Both are maintained and widely used. *These are future dependencies*
  — this ADR names them but does **not** add them to `go.mod`; the implementing
  batch adds the `require` lines and runs `go mod tidy`. A stdlib-only path
  (manual JWKS fetch + `crypto`-level RS256/EdDSA verification) is possible but
  re-implements discovery, key rotation, and claim validation by hand; we reject
  it as avoidable risk for no dependency saving worth the code.
- **Member provisioning — JIT.** The principal key is the IdP `subject` claim,
  matching `members.idp_subject` scoped by `org_id`. On a successful session
  exchange the control plane **upserts** the member by `(org_id, idp_subject)`:
  create on first login (JIT provisioning), otherwise refresh `email` /
  `display_name` from the token claims. New members default to `role = 'member'`
  (the column default); `admin` / `auditor` are granted out of band (admin UI or
  seed), never self-asserted from IdP claims in v1. A member with
  `active = false` is refused. IdP group→role mapping is a later enhancement
  (see out of scope).

### 2. Device-authorization flow (RFC 8628)

The CLI cannot open a browser-scoped session on its own, so it uses the device
grant. Lifecycle mapped onto the existing endpoints:

1. **`POST /auth/device/code` (`startDeviceAuthorization`).** The control plane
   generates a high-entropy opaque `device_code` and a short, human-typeable
   `user_code` (e.g. `BCDF-GHJK`, from an ambiguity-free alphabet). It stores a
   **pending-authorization record in Redis** and returns `device_code`,
   `user_code`, `verification_uri`, optional `verification_uri_complete`,
   `expires_in`, and `interval`.
2. **User approval.** The user opens `verification_uri` in a browser. That page
   is served by the admin UI and is **behind OIDC login** (§1), so approving the
   device request binds it to an authenticated member. On approval the record's
   state flips to `approved` and the member/org is recorded on it.
3. **`POST /auth/device/token` (`pollDeviceToken`).** The CLI polls with its
   `device_code` and `grant_type:
   urn:ietf:params:oauth:grant-type:device_code`. The control plane returns,
   per RFC 8628 and the contract's `OAuthError`:
   - `authorization_pending` (HTTP 400) while state is `pending`;
   - `slow_down` (HTTP 400) if the CLI polls faster than `interval` — the server
     also raises the effective interval by 5s;
   - `access_denied` if the user rejected;
   - `expired_token` once the record's TTL lapses;
   - on `approved`, a `200` `TokenResponse` (the session JWT, §3), after which
     the record is deleted so the code is single-use.

- **Where the state lives — Redis, with TTLs.** Each pending authorization is a
  Redis key `authz:device:<hash(device_code)>` (store only a hash of the
  `device_code`, never the raw value) holding state
  (`pending|approved|denied`), `user_code`, requested `client_id`/`scope`, the
  bound `member_id`/`org_id` once approved, and a last-poll timestamp for
  `slow_down` enforcement. TTL = `expires_in` (recommend **600s**). A second key
  maps `authz:user_code:<user_code> → device_code` so the browser approval step
  can find the record; it shares the same TTL. Redis TTLs give automatic
  expiry with no reaper job. `interval` recommend **5s**.

### 3. Token / session format — signed JWT + Redis revocation list

**Decision: short-lived signed JWTs for the access token, backed by a Redis
revocation list; long-lived opaque refresh tokens in Postgres.** This is the
honest reconciliation of the "opaque vs JWT" trade-off given that the contract
already pins `bearerFormat: JWT`.

- **Why JWT (not purely opaque).** The proxy and control plane can verify a JWT
  locally against a cached public key — no database round-trip on the hot path,
  which matters for per-request proxy checks. The contract, the admin UI
  exchange, and the device flow all already assume it.
- **Why not *stateless* JWT.** A purely stateless JWT cannot be revoked before
  expiry, which directly violates the threat model's revocation requirement. We
  recover opaque-token revocability by keeping a **revocation channel** in
  Redis.
- **Signing.** Asymmetric keys — **EdDSA (Ed25519)** or RS256 — so the proxy
  verifies with the *public* key and never holds signing material. The private
  key lives only in the control plane (config/secret store, out of scope here).
  `kid` in the JWT header supports key rotation.
- **Claims.** `iss` (control plane), `sub` (`idp_subject`), a member/org
  identifier, `org_id`, `roles`, `aud` (see below), `iat`, `exp`, and a unique
  `jti`. `sub` deliberately mirrors `members.idp_subject` so `/auth/whoami` and
  middleware resolve the `Member` by `(org_id, sub)`.
- **Audience.** Distinct audiences for the control-plane API vs the LLM proxy so
  a token minted for one cannot be silently replayed at the other; the proxy
  checks `aud` as the contract's whoami note already hints ("sanity-check token
  audience").
- **TTL & refresh.** Access-token TTL **short — recommend 15 minutes**. The
  `TokenResponse.refresh_token` (already optional in the contract) is a
  high-entropy **opaque** token stored **hashed in Postgres** (a new
  `auth_refresh_tokens` table: `id, member_id, token_hash, issued_at,
  expires_at, revoked_at, last_used_at`) with a longer TTL (recommend **30
  days**), rotated on each use (reuse of a rotated token revokes the family).
  The CLI refreshes via the token endpoint before expiry. *Adding this table is
  a migration owned by the implementing batch, not this ADR.*
- **Revocation propagation.** Revoking a member or session writes a
  **denylist entry to Redis** keyed by `jti` (or a per-member token epoch),
  with TTL = the access token's remaining lifetime. Middleware and the proxy
  consult this list; because access tokens are short-lived the denylist stays
  small and entries self-expire. Refresh tokens are revoked in Postgres
  (`revoked_at`), so a revoked session cannot mint a fresh access token. Net
  effect: a stolen token is cut off within, at most, one access-token TTL —
  satisfying the threat model.

### 4. CLI token storage

- **v1 — a `0600` file under the user config dir.** The CLI persists its tokens
  to `os.UserConfigDir()/drun-dash/credentials.json` (e.g.
  `~/.config/drun-dash/credentials.json` on Linux,
  `~/Library/Application Support/drun-dash/credentials.json` on macOS), created
  with mode **`0600`** in a `0700` directory. The file holds the `access_token`,
  `refresh_token`, `expires_at`, and the API base URL. This matches the CLI's
  existing env/default config style (`internal/cli/config_cmd.go`) — no new
  config subsystem is required.
- **Consistent with the threat model.** This is a *user session* token, not an
  LLM master credential — "no master credentials on the developer machine" is
  preserved. A stolen file yields only a short-lived, scoped, centrally
  revocable session (§3), which is exactly the "stolen laptop / stolen session
  token" case the threat model already accepts and bounds.
- **Future enhancement.** OS keychain storage (e.g.
  `github.com/zalando/go-keyring`: macOS Keychain, Windows Credential Manager,
  libsecret) behind the same storage interface. Not in v1.

### 5. Request authentication

- **Scheme.** Every protected request carries `Authorization: Bearer <jwt>`,
  matching the contract's `bearerAuth` scheme. This is the global default in
  `openapi/v1.yaml`; the four `/auth/*` mint/refresh endpoints keep
  `security: []` (unauthenticated).
- **Control-plane middleware.** A chi middleware (standard `http.Handler`)
  runs before protected routes and:
  1. extracts and parses the Bearer JWT;
  2. verifies the signature against the cached public key (by `kid`) and checks
     `iss`, `aud`, `exp`, `nbf` (small clock-skew leeway);
  3. checks the `jti`/epoch against the Redis revocation list (§3);
  4. loads the `Member` by `(org_id, sub)`, rejecting `active = false`;
  5. injects the resolved principal (member id, org id, roles) into the request
     `context.Context` for handlers and authorization checks.
  Failures return `401` (the contract's `Unauthorized` response).
- **Proxy.** The LLM proxy runs the same signature + `iss`/`aud`/`exp` +
  revocation check (steps 1–3) with the public key; it does **not** need the DB
  load for the token check itself, keeping the hot path fast (entitlement/spend
  enforcement is separate and out of scope here).
- **`/auth/whoami` (`getCurrentIdentity`).** Resolves the current `Member` from
  the middleware-injected principal and returns the contract's `Identity`
  (`subject` = `idp_subject`, `organization_id`, `email`, `roles`). No new
  contract change required.

## Consequences

**Positive**

- One agreed model: later batches (auth endpoints, CLI client, middleware,
  proxy check) implement against the same tokens, claims, and state locations.
- Fully consistent with `openapi/v1.yaml` — **no breaking contract changes**
  (the only additions are backing stores: a Redis key layout and a Postgres
  `auth_refresh_tokens` table, both owned by implementing batches).
- Meets the threat model's revocation requirement while keeping proxy
  verification cheap.
- No master credentials on the developer machine; stored token is short-lived,
  scoped, and revocable.

**Negative / trade-offs**

- Not purely stateless: the Redis revocation list is a dependency on the
  auth/proxy hot path. Mitigated by short TTLs (small list) and Redis already
  being required infrastructure. If Redis is unavailable, the safe default is
  **fail-closed** (deny), consistent with the threat model's "fail-closed
  offline" stance.
- JWTs are readable by the holder (base64, not encrypted); no secret is placed
  in claims — only identifiers, org, and roles.
- Signing-key management (generation, rotation, storage) becomes a control-plane
  operational concern; it is noted here but specified by a later batch.

## Alternatives considered

- **Opaque server-side session tokens (DB/Redis lookup per request).** Simplest
  revocation story, but forces a store round-trip on every proxy request and
  contradicts the contract's `bearerFormat: JWT`. Rejected; we instead take
  JWT + a narrow Redis revocation list to get most of the revocability without
  the per-request lookup.
- **Purely stateless JWT (no revocation list).** Cannot revoke before expiry;
  violates the threat model. Rejected.
- **Symmetric (HS256) signing.** Would require the proxy to hold the shared
  secret, widening blast radius. Rejected in favor of asymmetric keys.
- **Stdlib-only OIDC/JWKS validation.** Re-implements discovery and key rotation
  by hand for no meaningful dependency saving. Rejected in favor of
  `go-oidc`/`x/oauth2`.
- **Pre-provisioned members only (no JIT).** Higher admin friction with no
  security gain given IdP-verified subjects; JIT chosen, with roles still
  granted out of band.

## Out of scope for v1 (explicit)

- **SAML** SSO — deferred by the design; OIDC only in v1.
- **IdP group → role mapping / SCIM provisioning** — roles are granted out of
  band in v1.
- **OS-keychain CLI token storage** — `0600` file in v1; keychain is a later
  enhancement.
- **Multi-organization membership / org switching in a single session.**
- **Fine-grained OAuth scopes** beyond the coarse `member_role`.
- **Token binding / DPoP / mTLS-bound tokens.**
- **Signing-key rotation tooling and secret-store integration** (noted, not
  specified here).
- **Entitlement / spend enforcement at the proxy** — a separate concern from the
  token-authentication check described in §5.

## References

- `openapi/v1.yaml` — `Auth` paths, `bearerAuth` scheme, `TokenResponse`,
  `DeviceAuthorization*`, `OidcSessionExchangeRequest`, `Identity`, `OAuthError`.
- `migrations/000002_identity.up.sql` — `members`, `member_role`.
- `internal/redis/redis.go` — shared Redis access layer.
- `internal/cli/login.go`, `internal/cli/whoami.go` — CLI stubs (`TODO(batch/5)`).
- `docs/security/threat-model.md` — semi-trusted laptop, scoped short-lived
  tokens, prompt revocation, fail-closed offline.
- `docs/ARCHITECTURE.md`, `docs/TECH_STACK.md` — planes, locked stack.
- [RFC 8628](https://www.rfc-editor.org/rfc/rfc8628) — OAuth 2.0 Device
  Authorization Grant.
- [OpenID Connect Core](https://openid.net/specs/openid-connect-core-1_0.html).
