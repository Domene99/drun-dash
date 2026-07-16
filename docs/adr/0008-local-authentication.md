# 0008 — Local authentication (email/password, org self-signup, member invitations)

- **Status:** Proposed
- **Date:** 2026-07-15
- **Deciders:** drun-dash control-plane / admin-console work streams
- **Batch:** 26 (decision artifact; no code)

## Context

Authentication on `main` is **OIDC-only**. The `Auth` surface is
`POST /auth/session` (`exchangeOidcSession`) — the admin UI completes the
OIDC authorization-code flow with an external IdP and trades the resulting ID
token for a session JWT — plus the `POST /auth/device/*` device grant for the
CLI ([ADR-0001](0001-authentication-and-session-model.md)). There is **no way
to create an account or sign in without an external identity provider**:
`internal/ui` falls back to a **paste-a-token dev hack** (obtain a token
out-of-band with `drun-dash login` or a raw `POST /auth/session`, then paste it
into the login form), and orgs/members arrive only as seed data.

The product owner wants to run drun-dash **self-hosted, with no external IdP**:
create their own organization, become its Owner/super-admin, create agents,
**invite team members by email** (who set their own password in-app), assign
those members roles, and **sign in as them** to verify RBAC end to end. The
high-level direction — a **local email/password layer added alongside OIDC** —
is **approved**; this ADR locks the model, schema, API, and UI so the migration,
the store, the three new auth handlers, the invitation machinery, and the admin
console are built against one design. It writes **no code** and changes neither
`openapi/v1.yaml` nor the migrations; where the target diverges from `main`, it
records the change and names the follow-up, exactly as ADR-0003/ADR-0006 do.

What already exists and constrains this decision:

- **Token core — `internal/auth`.** `Issuer.Mint(ctx, auth.Claims{OrgID,
  MemberID, Roles, RegisteredClaims{Subject}})` mints the short-lived EdDSA
  session JWT (`org_id`, `mid`, `roles`, `sub`); `Verifier` + `authmw.RequireAuth`
  validate it and stash `*auth.Claims` in the request context. This package is
  **auth-mechanism-agnostic** — it mints from a claim set and neither knows nor
  cares whether those claims came from OIDC, the device flow, or a password
  check. All three new flows reuse it unchanged.
- **The existing OIDC handler — `internal/httpapi/auth.go`.** `exchangeOidcSession`
  resolves a member by `(org_id, idp_subject)` via
  `Members.GetByIDPSubject`, checks `member.Active`, and mints with
  `Roles: []string{string(member.Role)}` and `Subject: member.IDPSubject`. It
  returns a single generic `401` for every identity-resolution failure (no
  user/org enumeration). This is the template the password handlers follow.
- **Identity schema — `migrations/000002_identity.up.sql`.** `members(id,
  org_id, idp_subject TEXT NOT NULL, email TEXT NOT NULL, display_name, role
  member_role DEFAULT 'member', active BOOLEAN DEFAULT true, created_at)`,
  **`UNIQUE (org_id, idp_subject)`**. There is **no `password_hash` column, no
  uniqueness on `email`, and `idp_subject` is `NOT NULL`** — all three matter
  below (§1, §3).
- **Orgs — `internal/store/organizations.go`.** `OrganizationStore.Create(slug,
  name)` inserts one row and maps a duplicate slug to `ErrConflict`. It seeds
  **nothing else** — no roles, no members.
- **RBAC — [ADR-0006](0006-rbac-and-access-model.md) /
  `migrations/000007_rbac.up.sql`.** Every org is seeded with four **built-in**
  roles — **Owner** (`all_agents = true`, every permission), Admin, Auditor,
  Member — but **only by the migration, which seeds existing orgs** (`SELECT …
  FROM organizations`). Nothing seeds roles for an org created **at runtime**.
  Authorization is resolved from the DB per request through
  `requirePermission(perm)` / `authorizeRun`; `members.manage` governs member
  management and role assignment; roles reach a member via
  `member_roles`/`team_roles`. The JWT `roles` claim is identity metadata, **not**
  an authorization input.
- **Admin UI — `internal/ui`.** Server-rendered (templ + HTMX + `internal/uikit`).
  `GET /login` redirects to the IdP when OIDC is configured, else renders the
  paste-a-token form; the session JWT lives in the `dd_session` HttpOnly cookie.
  There is no signup, no password field, and no member-management surface.
- **Threat model — `docs/security/threat-model.md`.** Short-lived scoped tokens,
  prompt revocation, fail-closed, no secret in claims.

## Decision drivers

- **Additive, not a rewrite.** OIDC keeps working untouched; password auth is a
  second front door onto the **same** members, sessions, JWT, and RBAC. A member
  may end up with both (OIDC SSO *and* a password) or either alone.
- **One session model downstream.** All three new flows (signup, login,
  accept-invite) mint the **same** JWT via `internal/auth.Issuer`, so RBAC,
  `authmw`, revocation, and every handler are **unchanged** — the only new code
  is *how the claims are established*, never *what a session is*.
- **Self-hosting with zero external dependencies.** Signup + password login must
  work with only Postgres and Redis — no IdP, no mailer. The invite link is
  surfaced in-app and logged rather than emailed.
- **Never store or leak a password.** Hash with a slow, salted KDF; constant-time
  compare; generic errors that reveal neither which orgs/members exist nor
  whether the email or the password was wrong.
- **Reuse `main`'s grain.** New rows follow the existing org-scoped,
  composite-key style; the handlers follow `exchangeOidcSession`; the migration
  is additive and reversible.
- **Contract fidelity, honest gaps.** New endpoints and the `403`/`409` they add
  are named as OpenAPI follow-ups (the ADR-0002 posture), not silently shipped.

## Decision

### 1. Password auth is additive; a nullable `password_hash` on `members`

A member may authenticate via **OIDC** (existing enterprise SSO) **or**
**email+password** (new), and the two are not mutually exclusive. Add a
**nullable `password_hash TEXT`** column to `members`:

- **OIDC-provisioned members have `password_hash = NULL`** until they set one; a
  `NULL` hash simply means "this member cannot log in by password (yet)" and any
  password-login attempt against it fails with the generic auth error (§2),
  never a special case.
- **Password members** (created by signup or invite-acceptance) carry a bcrypt
  hash and may or may not also have an OIDC `idp_subject` linked later.

All three new flows end identically to `exchangeOidcSession`: resolve/create the
member, confirm `active`, and call `Issuer.Mint` with the member's `OrgID`,
`MemberID`, coarse `Roles`, and `Subject`. **The minted token is
indistinguishable from an OIDC-minted one** — same claims, same TTL, same
revocation, same `authmw`. Nothing downstream of the mint changes.

### 2. Password hashing — bcrypt

Hash with **`golang.org/x/crypto/bcrypt`** at **cost ~12** (a tuned constant,
not user-supplied). bcrypt is chosen over a bespoke construction because it is
salted, deliberately slow, battle-tested, and already idiomatic in Go; `x/crypto`
is a first-party module. *This ADR names the dependency but does not add it to
`go.mod`*; the implementing batch adds the `require` line and runs
`go mod tidy`.

- **Never store plaintext.** Only `bcrypt.GenerateFromPassword` output is
  persisted, in `members.password_hash`.
- **Constant-time verify.** `bcrypt.CompareHashAndPassword` on login; when
  `password_hash IS NULL` still perform a **dummy compare** against a fixed cost
  so a missing-hash member and a wrong-password member take the same time (no
  timing oracle for account existence).
- **Generic errors, no enumeration.** Signup duplicate, unknown email, wrong
  password, inactive member, and null hash all surface as the **same** generic
  `401` on login (and a generic `409` on signup for "already taken"), mirroring
  the OIDC handler's single flat `401`. Error bodies never say *which* factor
  failed.
- **Password policy** is a minimal server-side length floor (recommend ≥ 8, ≤ 72
  — bcrypt's byte ceiling — validated as a `400`), not a complexity regime.
- **Follow-up (explicit):** per-IP / per-account **rate limiting and lockout** on
  login and accept-invite, and password-strength/breach checks, are **not** in
  this ADR — named as a security follow-up so the first cut is not read as
  complete brute-force protection.

### 3. Identity model: email uniqueness and the local subject

Two schema facts on `main` collide with password login and must be reconciled.

**(a) Uniqueness rule — email unique *per org*; login carries the org.** Today
`members` is unique per `(org_id, idp_subject)` and email is **not** constrained.
Password login must resolve a member from `(email, password)`, which needs a
uniqueness rule. Two options were on the table:

- **(a) email unique per org**, login form asks for the **org slug/name**; or
- **(b) email globally unique across the instance.**

**Decision: (a).** An org is the tenancy boundary everywhere else in the system
(members, teams, roles, agents are all org-scoped), and the same human may
legitimately belong to two orgs under one email address; global uniqueness would
forbid that and leak cross-tenant existence ("this email is taken" across org
boundaries). So `POST /auth/login` takes **`{ org_slug, email, password }`**, the
org is resolved by slug first, and the member is looked up by
`(org_id, lower(email))`. The migration adds a **unique index on
`(org_id, lower(email))`** (case-insensitive; emails are compared lower-cased).
`signup` and `invite` enforce the same rule, so an org can never hold two members
with the same email.

**(b) The `sub` claim / `idp_subject NOT NULL`.** The token's `sub` mirrors
`members.idp_subject`, which is `NOT NULL` and unique per org. Password members
have **no** IdP subject, yet still need a stable, unique, non-null value so the
row satisfies the constraint and the token carries a `sub`. **Decision: synthesize
a local subject** — `idp_subject = "local:" <member_id>` (or another opaque,
per-instance-unique local prefix) for signup/invite members. This keeps
`idp_subject NOT NULL` and its unique index **unchanged** (no schema churn on that
column), namespaces local principals away from any real IdP subject, and lets the
`sub` claim stay populated. If that member later links OIDC, the linkage is a
follow-up (see out of scope); RBAC and `authmw` key off `mid` (the member id), so
the synthetic subject never participates in authorization. *(This makes email —
not subject — the login key; `idp_subject` remains the OIDC key.)*

### 4. Org self-signup (public)

**`POST /auth/signup { org_name, org_slug?, email, name, password }`** — the
public, unauthenticated entry point that lets the owner "create their own org and
become super-admin." In one transaction it:

1. **Creates the organization** (`OrganizationStore.Create`), deriving `org_slug`
   from `org_name` when omitted; a **duplicate slug is a `409`**.
2. **Seeds the four built-in roles** for the new org — Owner/Admin/Auditor/Member
   with the exact permission sets ADR-0006 §1 / migration `000007` define.
   **This is new runtime code:** the migration seeds only orgs that exist at
   migration time, so runtime org creation must seed roles itself. The seeding
   logic is factored so signup and the migration stay in lock-step (one source of
   truth for the built-in catalog); this is called out as an ADR-0006 follow-up.
3. **Creates the first member as Owner** — `active = true`, synthetic
   `idp_subject` (§3), `password_hash` set from the supplied password (§2),
   assigned the built-in **Owner** role via `member_roles` (super-admin: every
   permission, all agents). A **duplicate email within the org** is the same
   generic `409`.
4. **Mints a session** (`Issuer.Mint`) and returns a `TokenResponse` — the caller
   is immediately signed in as the org Owner.

Because the first member gets Owner, the ADR-0006 §7 guardrail (an org always has
at least one Owner) holds from creation, and there is no "org with no owner"
window.

### 5. Password login

**`POST /auth/login { org_slug, email, password }`** — resolve the org by slug,
load the member by `(org_id, lower(email))`, `bcrypt.CompareHashAndPassword`
against `password_hash`, confirm `active`, and mint a session. Every failure
(unknown org, unknown email, null hash, wrong password, inactive) returns the
**same generic `401`** with a constant-time path (§2). On success the response is
a `TokenResponse` identical in shape to the OIDC exchange.

### 6. Member invitations

Invitations let an Owner/Admin add teammates by email without a mailer, and let
those teammates set their own password.

**`POST /members/invite { email, role_ids }`** (permission **`members.manage`**,
per ADR-0006 §6) — in the caller's org:

- Create a **pending member**: `active = false`, `password_hash = NULL`,
  synthetic `idp_subject` (§3), the given `email` (rejected as `409` if the org
  already has that email). Pending members are inactive, so even though the row
  exists they cannot authenticate by any means until acceptance.
- Create an **invitation token**: a high-entropy random value (e.g. 32 bytes,
  URL-safe), returned to the caller **once** in cleartext as part of an **invite
  link**, and stored **only hashed** (SHA-256 — the token is high-entropy, so a
  fast hash is sufficient and a bcrypt round is unnecessary) in a new
  `invitations` table with a **TTL** (recommend **7 days**) and the
  **pre-selected `role_ids`**.
- **Local dev — no email is sent.** The response returns the invite link; the
  admin UI displays it (copy-to-clipboard) and it is **logged**. A **mailer is an
  explicit follow-up**; the token machinery is designed so a mailer later just
  becomes another delivery channel for the same link.

**`POST /auth/accept-invite { token, name, password }`** (public,
unauthenticated) — hash the presented token, look it up, and if it is **unexpired
and unconsumed**: set the member's `display_name` and `password_hash` (§2),
**activate** the member (`active = true`), **assign the invited roles**
(`member_roles` from the token's stored `role_ids`), **mint a session**, and
**consume the token** (mark it used — **single-use**). An invalid, expired, or
already-consumed token is a generic error that reveals nothing. The pre-selected
roles come from the invitation, not from the acceptor, so a teammate cannot
self-escalate at acceptance.

### 7. Admin UI surfaces (`internal/ui`)

The admin console gains local-auth surfaces, keeping OIDC available when
configured:

- **`GET /signup`** — a "create your organization" page (org name, your email,
  name, password) that POSTs to the control plane's `/auth/signup`, stores the
  returned JWT in the `dd_session` cookie, and lands the new Owner on the
  dashboard. This is the owner's front door on a fresh instance.
- **Password `/login` becomes the primary local login.** `GET /login` renders an
  **email + org + password** form posting to `/auth/login`. When OIDC is
  configured, an **"Sign in with your identity provider"** button remains
  alongside it. The **paste-a-token form is demoted to a debug aid** (behind an
  explicit toggle / dev-only affordance), no longer the default local path.
- **Invite member** — a member-management surface (gated on the caller holding
  `members.manage`) with an **Invite** action that calls `/members/invite`,
  pre-selecting roles, and then **displays the returned invite link** for the
  admin to hand off (since no email is sent). An **accept-invite** page consumes
  the link, collects name + password, and posts to `/auth/accept-invite`.

The cookie/session model is unchanged (`dd_session`, HttpOnly, `Secure` in
production); only the ways to obtain the JWT grow.

### 8. Schema (new migration — additive, reversible; shape fixed here)

A **new migration** (implemented later — this ADR fixes the shape) adds:

```
ALTER TABLE members ADD COLUMN password_hash TEXT;      -- nullable (§1)

-- Email unique per org, case-insensitive (§3a). Enables password login by
-- (org_id, lower(email)); OIDC members are unaffected.
CREATE UNIQUE INDEX members_org_email_lower_idx
    ON members (org_id, lower(email));

CREATE TABLE invitations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    member_id   UUID NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL,                 -- SHA-256 of the token; never the raw value
    role_ids    UUID[] NOT NULL DEFAULT '{}',  -- pre-selected roles assigned on acceptance
    expires_at  TIMESTAMPTZ NOT NULL,          -- TTL (recommend 7 days)
    consumed_at TIMESTAMPTZ,                   -- non-null once accepted (single-use)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX invitations_token_hash_idx ON invitations (token_hash);
CREATE INDEX invitations_member_id_idx ON invitations (member_id);
```

Notes: the **existing unique index on `(org_id, lower(email))` must hold on the
current data** — a migration precondition (dedupe any pre-existing per-org email
collisions first, expected to be none given OIDC subjects, not emails, were the
key). `role_ids` as a `UUID[]` on the invitation is acceptable because the set is
small, written once, and read once at acceptance; it is not an authorization
source (the real grants land in `member_roles`). The `down` migration drops
`invitations`, the email index, and the `password_hash` column. Whether pending
members are stored as normal `members` rows (chosen here, so acceptance is a
plain update and the roster shows "invited") versus a separate table is settled
in favor of reuse; a `pending`/`active` flag already exists (`members.active`).

### 9. API surface (new — proposed, OpenAPI follow-ups)

All under the `Auth`/members tags, org-scoped per ADR-0002:

- **`POST /auth/signup`** — public; `{org_name, org_slug?, email, name,
  password}` → `TokenResponse`. `409` on duplicate slug/email; `400` on
  validation.
- **`POST /auth/login`** — public; `{org_slug, email, password}` →
  `TokenResponse`. Generic `401` on any failure.
- **`POST /auth/accept-invite`** — public; `{token, name, password}` →
  `TokenResponse`. Generic error on invalid/expired/consumed token.
- **`POST /members/invite`** — requires `members.manage`; `{email, role_ids}` →
  the pending member + invite link. `409` on duplicate email; `404` on a
  cross-org/unknown `role_id` (ADR-0002).

These operations, their request/response schemas, and the new `403`/`409`
responses are **OpenAPI additions** — named here, added by the implementing
batch, not made in this ADR. The three unauthenticated `/auth/*` endpoints keep
`security: []`, matching the existing mint endpoints (ADR-0001 §5).

## Consequences

**Positive**

- The owner can stand up drun-dash with **no external IdP or mailer**: sign up,
  become Owner, invite teammates, assign roles, and sign in as them to verify
  RBAC — the exact workflow requested.
- **Zero downstream change:** all three flows mint the same JWT via
  `internal/auth.Issuer`, so RBAC, `authmw`, revocation, and every handler are
  untouched. The new surface is entirely "how claims are established."
- **OIDC is preserved**, not replaced; a member can have SSO, a password, or
  both. The nullable `password_hash` makes the two coexist without a discriminator
  column.
- Passwords are bcrypt-hashed, constant-time-compared, and never enumerable;
  invitation tokens are single-use, TTL'd, and stored only hashed.
- The migration is additive and reversible; email-per-org uniqueness matches the
  system's tenancy grain and the synthetic local subject avoids any change to the
  `idp_subject` constraint.

**Negative / trade-offs**

- **New runtime role-seeding** (§4.2) duplicates the migration's built-in-role
  catalog in Go; the two must stay in lock-step. Mitigated by factoring one
  shared seeder and naming it an ADR-0006 follow-up — but it is real coupling.
- **New auth endpoints and `403`/`409`s the contract does not yet document** —
  carried as an explicit OpenAPI follow-up (§9), the ADR-0002 posture.
- **No rate limiting/lockout or mailer in v1** (§2, §6). Named as follow-ups so
  the first cut is not mistaken for complete brute-force protection or a real
  invite-delivery pipeline. Until the mailer lands, invite links pass through an
  admin's hands (surfaced + logged), which is acceptable for the self-hosted
  local-dev target but not for untrusted multi-tenant use.
- **A new dependency** (`golang.org/x/crypto/bcrypt`) enters `go.mod` in the
  implementing batch.
- **Email-per-org uniqueness** means the same person in two orgs holds two
  independent member rows/passwords; deliberate (tenancy boundary), but it is not
  single-sign-on across orgs.

## Alternatives considered

- **Replace OIDC with password auth.** Rejected: enterprise SSO is a real
  requirement (ADR-0001) and the owner asked for password auth *alongside* it,
  not instead. Additive keeps both.
- **A separate `credentials` / `auth_methods` table** instead of a column on
  `members`. Rejected for v1: one nullable `password_hash` is the minimal change,
  matches "a member optionally has a password," and avoids a join on every login.
  A dedicated table becomes worthwhile only once multiple password credentials or
  many auth-method types per member exist — a later ADR if needed.
- **Email globally unique across the instance** (option (b) in §3). Rejected: it
  forbids the same person belonging to two orgs and leaks cross-tenant existence.
  Email-per-org + an org slug on the login form matches the tenancy model.
- **Making `idp_subject` nullable** to represent password members. Rejected in
  favor of a synthetic `local:<member_id>` subject: it leaves the `NOT NULL` +
  unique constraint (and the `sub` claim contract) untouched and keeps every
  member row carrying a stable subject, at the cost of one derived value.
- **Argon2id / scrypt instead of bcrypt.** Reasonable alternatives; bcrypt chosen
  for being the simplest well-understood default with a first-party `x/crypto`
  implementation and no tuning surface beyond cost. A later ADR can migrate the
  KDF (the column stores an algorithm-tagged hash, so re-hashing on next login is
  possible) if memory-hardness is required.
- **Emailing the invite link now.** Rejected for this batch: a mailer is
  infrastructure the self-hosted target does not require to function; surfacing +
  logging the link unblocks the workflow, and the token design already
  accommodates a mailer as a later delivery channel.
- **Bcrypt-hashing the invitation token.** Rejected: the token is high-entropy
  random, so a fast SHA-256 with a unique index is sufficient and far cheaper;
  bcrypt is reserved for low-entropy human passwords.

## Out of scope for this ADR (explicit)

- **Any OpenAPI or migration change.** The endpoints, schemas, the
  `password_hash` column, the email index, and the `invitations` table are
  **proposals** for later batches, not changes made here.
- **Rate limiting, lockout, CAPTCHA, and password-strength/breach checks** on
  signup/login/accept-invite — a named security follow-up.
- **An email/SMTP mailer** for invitations, password reset, and email
  verification — the invite link is surfaced + logged in v1.
- **Password reset / "forgot password" and email-verification** flows — the same
  token machinery (§6) will host them; not specified here.
- **Linking a password to an existing OIDC member, or an OIDC login to a password
  member** (account linking) — password members carry a synthetic subject; real
  linking is a later ADR.
- **The concrete handler/store signatures, the shared built-in-role seeder's Go
  types, and cookie/CSRF specifics** — owned by the implementing batch; this ADR
  fixes their properties, not their code.
- **Session/refresh-token changes** — signup/login/accept-invite mint the same
  access token ADR-0001 §3 defines; refresh behavior is unchanged.

## References

- [ADR-0001](0001-authentication-and-session-model.md) — Bearer JWT session
  model, `internal/auth.Issuer`, the OIDC exchange and device flow, prompt
  revocation. Password auth is a new claim source for the same token.
- [ADR-0002](0002-rest-api-conventions.md) — org scoping, `403`-vs-`404`,
  naming contract↔behavior gaps as follow-ups.
- [ADR-0006](0006-rbac-and-access-model.md) — built-in roles, the permission
  catalog, `members.manage`, `member_roles`; the runtime role-seeding and Owner
  guardrail this ADR leans on.
- `internal/auth/token.go` — `Issuer.Mint`, `Claims{OrgID, MemberID, Roles}`.
- `internal/httpapi/auth.go` — `exchangeOidcSession` (the handler template) and
  its single generic `401`.
- `internal/httpapi/authz.go` — `requirePermission` / `authorizeRun`.
- `internal/store/organizations.go` — `OrganizationStore.Create` (slug →
  `ErrConflict`); `internal/store/roles.go` — `RoleStore`.
- `migrations/000002_identity.up.sql` — `members`, `idp_subject NOT NULL`,
  `UNIQUE (org_id, idp_subject)`; `migrations/000007_rbac.up.sql` — built-in
  role seeding this ADR mirrors at runtime.
- `internal/ui` — the admin console, current OIDC + paste-a-token login this ADR
  extends with `/signup`, password `/login`, and the invite surfaces.
- `openapi/v1.yaml` — the `Auth` and members operations the new endpoints extend.
- `docs/security/threat-model.md` — no plaintext secrets, generic errors,
  fail-closed.
- [ADR-0005](0005-design-system-and-information-architecture.md),
  `docs/ARCHITECTURE.md`, `docs/TECH_STACK.md` — the shared uikit design system
  and locked stack the new pages compose within.
