# 0003 — Session launch and scoped credential-grant model

- **Status:** Proposed
- **Date:** 2026-07-11
- **Deciders:** drun-dash control-plane / proxy / CLI work streams
- **Batch:** 10 (decision artifact; no code)

## Context

Session launch is the platform's central flow and the largest remaining
feature. `launchSession` ties together five subsystems that already exist on
`main` but have never been wired end to end: **entitlements** (who may run which
agent), **policy versions** (the governance boundary), the **credential vault**
(custody of the master LLM keys), **scoped proxy tokens** (`credential_grants`),
and **metering** (`usage_events`). The drun engine on the developer's laptop is
the consumer — it receives a signed policy bundle and a scoped proxy token and
runs entirely against them.

Several later batches will implement pieces of this flow in parallel — the
`launchSession`/`terminateSession` handlers, the proxy's per-call token check
and metering, and the CLI launch client. If each picks its own answer for "how
is entitlement decided," "what exactly is signed and by which key," or "how does
the proxy validate a token," the pieces will not compose. This ADR **decides the
flow once**, grounded in what is already on `main`, so those implementations
stay consistent. It writes **no code** and changes neither `openapi/v1.yaml` nor
the migrations; where the contract and the code on `main` genuinely disagree, it
records the behavior and names an explicit follow-up, exactly as
[ADR-0002](0002-rest-api-conventions.md) does.

What already exists and constrains this decision:

- **API contract — `openapi/v1.yaml`.**
  - `POST /sessions/launch` (`launchSession`) takes `SessionLaunchRequest`
    (`agent_id` required; optional `agent_version` pin, `requested_fan_out`,
    `labels`) and returns **`201`** `SessionLaunchResponse`
    (`session_id`, `policy_bundle`, `proxy_base_url`, `proxy_token`,
    `fan_out_ceiling`, optional `expires_at`). `403` is a documented inline
    response: "The caller is not entitled to launch this agent." `400`, `401`,
    `501` are the reusable responses.
  - `SessionLaunchResponse.policy_bundle` is a `PolicyBundle`
    (`policy_id`, `version`, `boundary`, `signing`). `boundary` is a
    `PolicyBoundary` (`allowed_tools`, `network`, `fan_out_ceiling`, `rules`);
    `signing` is `SigningMetadata` (`algorithm`, `key_id`, `signature`,
    `signed_at`), described as a **detached signature proving policy provenance,
    verified by the client at launch**.
  - `proxy_token` is "scoped, short-lived token the LLM proxy accepts for this
    session"; `proxy_base_url` is the proxy the session must route through.
  - `POST /sessions/{sessionId}/terminate` (`terminateSession`) "terminate[s] a
    running session and revoke[s] its proxy credentials. Idempotent once the
    session has reached a terminal state." The API preamble states the LLM proxy
    "validates the `proxy_token` minted at session launch."
- **Schema — `migrations/000004`–`000005`.**
  - `sessions` (`org_id`, `agent_id` `ON DELETE RESTRICT`, nullable
    `agent_version`, `policy_version_id`, `environment_id`, `launched_by`;
    `status session_status` default `pending`; `fan_out_ceiling` default 1,
    `CHECK >= 0`; `labels` jsonb). `session_status` enum is
    `pending|active|terminated|failed`.
  - `credential_grants` (`session_id` `ON DELETE CASCADE`, **`proxy_token_hash`**
    — "the raw token is never stored", nullable `endpoint_id`, `scopes` jsonb
    default `[]`, `issued_at`, `expires_at` **NOT NULL**, `revoked_at`). A
    **unique index on `proxy_token_hash`**; indexes on `session_id` and
    `expires_at`.
  - `usage_events` (append-only; `org_id`, nullable `session_id`, `agent_id`,
    `endpoint_id`, `model`, `prompt_tokens`, `completion_tokens`, generated
    `total_tokens`, `cost_amount`; `id` is a `BIGINT IDENTITY`).
  - `agent_versions` pins `policy_version_id` (`ON DELETE RESTRICT`),
    `endpoint_id` (`ON DELETE RESTRICT`), `budget_id`, `fan_out_ceiling`.
  - `entitlements` grant an agent to a **member XOR team**, with an optional
    `policy_version_id` override and optional `max_fan_out`; unique per
    `(agent_id, member_id)` / `(agent_id, team_id)`. `team_memberships` is the
    member↔team join.
  - `model_endpoints` carries `secret_ref` — "an opaque … pointer at an external
    secret store"; the master key is never in the row.
- **Stores on `main` (`internal/store`).** `SessionStore` (`Create` with
  `CreateSessionParams`, `GetByID`, `ListByOrg`, `SetStatus`);
  `CredentialGrantStore` (`Create` storing only `ProxyTokenHash`,
  `GetByTokenHash` — "the proxy's per-call token lookup; callers must still check
  ExpiresAt and RevokedAt", `Revoke`, `ListBySession`); `UsageEventStore`
  (`Insert`, `ListByOrgBetween`); `EntitlementStore` (`ListByAgent`,
  `ListByMember`, `ListByTeam`); `AgentVersionStore`
  (`GetByAgentAndVersion`); `PolicyVersionStore` (`GetByPolicyAndVersion`);
  `AgentStore`, `ModelEndpointStore`, `TeamMembershipStore` (member↔team).
- **`internal/vault`.** `Vault.Resolve(ctx, secretRef) → (Secret, error)`; the
  `Secret` value is redacted in `String`/`GoString`. `EnvVault` is the dev
  implementation.
- **`pkg/policy`.** The **signed drun policy-bundle format**: `PolicyBundle`
  (`schema_version`, `org_id`, `policy_id`, `policy_version`, **`drun_config`**,
  `issued_at`, `expires_at`, `content_hash` = `sha256:` of the canonical
  `drun_config`, `signature` = `ed25519:`+base64). `Load` (strict decode),
  `CheckFreshness`, `VerifyContentHash`, `VerifySignature(pub)`, and the
  fail-closed `Verify(now, pub)`; `DrunConfig.ToTOML()` compiles the config;
  `SigningInput()` is a domain-tagged (`drun-policy-bundle/v1`) ASCII join of the
  identity + freshness + `content_hash` fields. Signing-key management is
  explicitly **left to a later batch** (`docs/policy/DRUN_POLICY_BUNDLE.md`).
- **`internal/proxy` (llm-proxy).** Today the broker resolves **one** upstream
  credential from a fixed, env-configured `secret_ref`
  (`vault.Resolve`) and injects it as `Authorization: Bearer` while streaming
  passthrough. It does **not yet** validate a `proxy_token` or look up a grant —
  that per-call path is what this ADR designs.
- **ADR-0001 / ADR-0002.** Auth is a Bearer JWT; the middleware injects the
  principal (member id, `org_id`, roles) into `context.Context`. Handlers derive
  the org **only** from that principal; cross-org id references return **`404`**
  (never `403`); `403`/`409` are emitted **only** where the contract documents
  them. The threat model requires **fail-closed**, short-lived, per-session
  scoped tokens, and prompt revocation; the laptop **never holds a master key**.

## Decision drivers

- **Custody + scoping + revocation is the product.** The threat model's whole
  leverage is that the master LLM key lives only in the proxy and the laptop
  gets a short-lived, narrowly-scoped token. The launch flow must preserve that
  invariant end to end.
- **Fail-closed.** No valid bundle → no credentials → the sandbox runs offline.
  A partial launch must never leave a usable token without an enforceable
  policy, or a session row without its grant.
- **Contract fidelity, honest gaps.** Emit the shapes `openapi/v1.yaml`
  documents; where `main`'s code (notably `pkg/policy.PolicyBundle`) and the
  contract's `PolicyBundle` genuinely diverge, record the behavior and name the
  contract follow-up rather than silently diverge — the ADR-0002 rule.
- **Reuse what `main` provides.** Prefer the existing stores, `vault.Resolve`,
  and `pkg/policy` over inventing parallel machinery.
- **Cheap, safe proxy hot path.** The proxy's per-call token check is on the
  critical path; it must be a single indexed lookup plus a vault resolve, and it
  must fail closed on expiry/revocation.
- **Consistency across parallel batches.** One rule each for entitlement
  resolution, bundle issuance, and the grant lifecycle, so the launch handler,
  the proxy, and the CLI reach the same behavior without coordinating.

## Decision

### 1. Entitlement resolution and authorization

`launchSession` runs the following resolution, entirely from the ADR-0001
principal (member id + `org_id`) — never from the request body:

1. **Load the agent, org-scoped.** Look up `agent_id`. If it does not exist, or
   its `org_id` differs from the caller's, return **`404 not_found`** — the
   ADR-0002 tenant-isolation rule; a cross-tenant agent is indistinguishable
   from "does not exist." All later steps operate on an agent already confirmed
   to be in the caller's org.
2. **Select the agent version.** If `SessionLaunchRequest.agent_version` is set,
   resolve that exact `(agent_id, version)` via
   `AgentVersionStore.GetByAgentAndVersion`; a missing pin is **`404`**
   (nested-resource-under-owned-parent, ADR-0002). If unset, use the agent's
   `current_version` (the "latest" the contract's default refers to); an agent
   with `current_version = 0` (no published version) is **`404`** — there is
   nothing runnable to launch.
3. **Check entitlement (member XOR team).** The caller is entitled iff an
   `entitlements` row exists for this agent that matches the caller: either
   `member_id = caller` **or** `team_id ∈ {caller's teams}` (from
   `TeamMembershipStore`). Resolved via `EntitlementStore.ListByAgent(agentID)`
   intersected with the caller's `{member_id} ∪ team_ids`. If **no** entitlement
   matches, return **`403 forbidden`** (`httpx.CodeForbidden`) — the one place
   the contract documents `403`, precisely for "not entitled to launch this
   agent."
4. **Resolve the effective policy version.** Default to the selected
   `agent_version.policy_version_id`; if the **matched entitlement** carries a
   `policy_version_id` override, that wins (the column's documented purpose —
   "optional policy-version override for this grant"). The chosen
   `policy_version_id` is stamped on the session row and drives §2. An agent
   version with a null `policy_version_id` and no override is a
   misconfiguration, not a client error → **`500`** (logged), never a silent
   unbounded launch.
5. **Resolve the endpoint.** The upstream is the selected
   `agent_version.endpoint_id` (`model_endpoints`, `ON DELETE RESTRICT` so it is
   present). It is stamped on the grant (§3) and is what the proxy will
   `vault.Resolve` at call time. A null `endpoint_id` is likewise a `500`.

**`403` vs `404` — the rule.** Cross-org / non-existent agent, or a missing
version pin, → **`404`** (leak nothing across tenants). An agent that exists **in
the caller's own org** but that the caller is not entitled to → **`403`**.
Returning `403` here leaks nothing new: `listAgents` already exposes every agent
in the caller's org to any member, so "this agent exists but you may not run it"
reveals no secret, and the contract explicitly reserves `403` for exactly this
case. This is fully consistent with ADR-0002: `404` for cross-tenant id
references, `403` only where the contract documents it.

**Fan-out ceiling — clamp, never expand.** The effective ceiling returned as
`fan_out_ceiling` (and stored on the session) is the **minimum** of every bound
that applies: `SessionLaunchRequest.requested_fan_out` (when set), the selected
`agent_version.fan_out_ceiling`, the policy boundary's `fan_out_ceiling`, and the
matched entitlement's `max_fan_out` (when set). Absent bounds are ignored;
`requested_fan_out` is clamped **down** to the tightest of the rest and never
raises it. This mirrors the request field's own description ("clamped to the
policy and entitlement ceilings").

### 2. Policy-bundle issuance

At launch the control plane turns the pinned **policy version** into a signed,
freshness-bound **`pkg/policy.PolicyBundle`** and hands it to the client, which
verifies it fail-closed before it runs anything.

- **Two representations, one compile step.** The stored governance artifact
  (`policy_versions.content_json`) is the declarative **`PolicyBoundary`**
  (`allowed_tools`, `network`, `fan_out_ceiling`, `rules`). The artifact the
  drun engine enforces is a **`pkg/policy.DrunConfig`** (`domain_allowlist`,
  `mount_allowlist`, `bash_command_*`, timeouts, …) that compiles one-to-one
  into `~/.drun/config.toml`. These are different vocabularies — governance
  intent vs sandbox enforcement — so launch performs a deterministic
  **compile**: `PolicyBoundary → DrunConfig` (e.g. `network`/`allowed_tools` →
  `domain_allowlist`/`bash_command_allowlist`, `rules` → the remaining knobs).
  The exact field mapping is owned by the implementing batch; this ADR fixes
  **that** the compile happens at issuance and is deterministic (identical
  boundary → identical `drun_config` → identical `content_hash`).
- **Who signs, with which key.** The control plane assembles the
  `pkg/policy.PolicyBundle` — `org_id`, `policy_id`, `policy_version` from the
  resolved version; `drun_config` from the compile; `content_hash` =
  `sha256:`+hex of the canonical `drun_config`; `issued_at = now`, `expires_at =
  now + bundle TTL` (§4) — and **signs `SigningInput()` with the control plane's
  Ed25519 bundle-signing key**. This is a distinct key from the ADR-0001 session
  **JWT** signing key (different purpose, different rotation), identified by a
  `kid`. The proxy and clients hold only the **public** key; the private key
  lives solely in the control plane (key management is explicitly a later batch,
  per `pkg/policy` and ADR-0001 — this ADR only fixes *which* signature covers
  *what*). Signing the `pkg/policy` `SigningInput` (domain-tag + identity +
  freshness + `content_hash`) means one signature authenticates the whole
  bundle, including the compiled config.
- **How the client consumes it.** The client runs the fail-closed pipeline
  `PolicyBundle.Verify(now, pub)` — structural validation, freshness,
  `content_hash`, Ed25519 signature — and, **only if every step passes**,
  compiles `DrunConfig.ToTOML()` to `~/.drun/config.toml` and begins using the
  proxy token. Any failure → the sandbox runs offline (no bundle → no
  credentials), satisfying the threat model. Rollback protection (reject a
  `policy_version` older than the newest already accepted) is the client's
  responsibility, as `pkg/policy.Verify` documents.
- **Reconciliation with the OpenAPI `PolicyBundle` — a named gap.** The
  contract's `PolicyBundle` models `{policy_id, version, boundary, signing}`: it
  carries the **boundary** (declarative) and a `SigningMetadata` block, but
  **not** the `drun_config`, `issued_at`/`expires_at`, or `content_hash` that
  `pkg/policy.Load`/`Verify` require, and its `signing.signature` is described as
  a signature over the *boundary digest*, whereas the enforceable signature is
  over the *`drun_config`-derived* `SigningInput`. The engine cannot run from the
  contract shape alone. **Decision:** the launch response must deliver a
  **verifiable `pkg/policy.PolicyBundle`**; the handler maps what the contract
  field can hold (`policy_id`, `version`, `boundary` from `content_json`,
  `signing` = `{algorithm: "ed25519", key_id: kid, signature, signed_at:
  issued_at}`) and this is recorded as a **contract follow-up**: extend the
  OpenAPI `PolicyBundle` (and `SigningMetadata`) to carry the compiled,
  freshness-bound, content-hashed bundle — i.e. add `schema_version`,
  `drun_config`, `issued_at`, `expires_at`, `content_hash`, or embed the
  serialized `pkg/policy` bundle — so the delivered artifact and the contract
  agree. This mirrors ADR-0002's handling of `manifest_digest`: keep the honest
  artifact, name the reconciliation, do not silently ship a shape the consumer
  cannot verify.

### 3. Scoped proxy credential grant

The `proxy_token` is a **short-lived, opaque, per-session bearer secret** whose
only stored form is a SHA-256 hash. Lifecycle:

- **Mint (at launch).** Generate a high-entropy token — **32 bytes from
  `crypto/rand`, base64url**, prefixed for legibility (e.g. `dgp_…`, "drun grant
  proxy"). Compute `sha256(raw)` and `CredentialGrantStore.Create` a row with
  `session_id`, `proxy_token_hash`, `endpoint_id` (§1.5), `scopes` (below), and
  `expires_at = now + grant TTL` (§4). The **raw token is returned exactly once**
  in `SessionLaunchResponse.proxy_token` and is **never persisted** — a database
  leak exposes only hashes, and the unique index on `proxy_token_hash` gives the
  proxy an O(1) lookup.
- **Why opaque, not a JWT.** Unlike the ADR-0001 control-plane session token
  (JWT, so the proxy can check it without a DB round-trip), the proxy token
  **must** hit the DB anyway — to find the grant's `endpoint_id`, `scopes`,
  expiry, and revocation state — so a self-verifying JWT buys nothing and would
  make revocation harder. An opaque random token is trivially revocable (stamp
  `revoked_at`) and carries no readable claims. A distinct **audience** from the
  control-plane JWT is implicit: the two tokens are validated by entirely
  different paths and are not interchangeable.
- **Why SHA-256 (not a password KDF).** The token is 256 bits of `crypto/rand`
  entropy, so there is nothing to brute-force; a fast hash is correct here and
  keeps the unique-index lookup exact. (Contrast ADR-0001's refresh tokens,
  hashed for the same reason.)
- **`scopes`.** A small JSON structure recording what the grant authorizes at
  the proxy — the pinned `endpoint_id`, and the permitted operation/model set
  derived from the policy boundary (e.g. `{"endpoint_id": "…", "operations":
  ["chat.completions"], "models": [...]}`). The proxy enforces it; the exact
  shape is the implementing batch's, but it is **derived from policy, not
  client-supplied**, so a stolen token cannot widen its own scope.
- **Proxy validation (per call).** The broker, on each request: (1) reads the
  presented bearer token; (2) `sha256`-hashes it and calls
  `CredentialGrantStore.GetByTokenHash` — miss → **`401`**; (3) checks
  `revoked_at IS NULL` **and** `expires_at > now` (and that the owning session is
  not terminated) — otherwise **`401`**, fail-closed; (4) enforces `scopes`
  against the requested operation/model — violation → **`403`**; (5) only then
  `vault.Resolve(endpoint.secret_ref)` and injects the real upstream key as
  `Authorization: Bearer`, overwriting any client value, and streams passthrough.
  This is the natural extension of today's broker, which already resolves a
  fixed `secret_ref` and injects it; the change is that the `secret_ref` now
  comes from the grant's `endpoint_id` and every call is gated by the grant. The
  master key never leaves the proxy.
- **Revocation & termination.** `terminateSession` sets the session
  `status = terminated` (`SessionStore.SetStatus`) **and** revokes every grant
  for the session (`ListBySession` → `Revoke`), so in-flight tokens stop working
  within one proxy call. It is **idempotent**: terminating an
  already-terminal session re-stamps `revoked_at`/status and returns `200` (the
  contract's idempotency note). Because `credential_grants.session_id` is
  `ON DELETE CASCADE`, grants also vanish if the session is ever deleted, but
  termination is the live path and uses explicit revocation so the proxy's
  `revoked_at` check is authoritative.

### 4. Atomicity, session lifecycle, and TTLs

- **One transaction, fail-closed.** Resolution (§1) is read-only; the mutation —
  insert the `sessions` row and insert the `credential_grants` row — runs in a
  **single DB transaction**, and the bundle (§2) is **signed before** the
  response is written. If signing fails, or either insert fails, the transaction
  rolls back and `launchSession` returns an error (`500`, or the mapped store
  error) with **no** session and **no** grant persisted. This guarantees the
  threat-model invariant: a token is never handed out without a durable,
  enforceable grant, and a session never exists without its credential.
- **Status on launch.** A successful launch creates the session as **`active`**:
  bundle signed + grant minted **is** the activation, and there is no separate
  "activate" callback in the contract. `pending` is reserved for a future
  async-provisioning path; `failed` is for a launch that aborts **after** the
  row is committed (not the rollback case above); `terminated` is reached only
  via `terminateSession`. `getSession` reports this status.
- **TTLs — one session lifetime governs both.** The **bundle** `expires_at`
  (§2) and the **grant** `expires_at` (§3) are set from a single **session TTL**
  (recommend a configurable default of **60 minutes**), so the policy the client
  enforces and the credential it presents expire **together** — a session cannot
  outlive its policy or vice versa. `SessionLaunchResponse.expires_at` reports
  this instant. The contract defines no renew endpoint, so **renewal is out of
  scope**: a long session re-launches (new bundle, new grant) rather than
  extending in place; a renewal endpoint is a named future enhancement.

### 5. Metering seam

Attribution flows from the grant. On each forwarded call the proxy resolves the
grant → `session_id` → session (`org_id`, `agent_id`), reads token counts from
the upstream response, and `UsageEventStore.Insert`s an append-only
`usage_events` row (`org_id`, `session_id`, `agent_id`, `endpoint_id`, `model`,
`prompt_tokens`, `completion_tokens` — `total_tokens` is generated,
`cost_amount`). This is the **seam**, not the policy: **budget enforcement**
(reading `budgets`/`budget_rules`, blocking on a hard cap) is a separate concern
and a later batch; this ADR fixes only that usage is attributed through the
grant's session so spend analytics (`ListByOrgBetween`) and future enforcement
have faithful data.

## Consequences

**Positive**

- One agreed launch flow: the `launchSession` handler, the proxy's token check +
  metering, and the CLI client implement against the same resolution order,
  signed artifact, and grant lifecycle without re-litigating any of it.
- The custody invariant holds end to end: the laptop receives only a
  short-lived, scoped, revocable token; the master key never leaves the proxy;
  revocation at `terminateSession` cuts access within one call — exactly the
  threat model's promise.
- Fail-closed throughout: no valid bundle → no config → offline; a partial
  launch rolls back so a token never exists without an enforceable grant.
- Reuses `main` wholesale — `SessionStore`, `CredentialGrantStore`,
  `UsageEventStore`, `EntitlementStore`, `vault.Resolve`, and the `pkg/policy`
  verify/compile pipeline — with no new infrastructure.

**Negative / trade-offs**

- The delivered policy artifact and the OpenAPI `PolicyBundle` shape do not yet
  match: the engine needs the `pkg/policy` bundle (`drun_config` + freshness +
  `content_hash`), which the contract's `PolicyBundle` omits. This is carried as
  an explicit contract follow-up (§2), a debt, not a handler bug — the same
  posture ADR-0002 took on `manifest_digest`.
- The proxy's per-call check is a DB lookup (`GetByTokenHash`) plus a vault
  resolve on the hot path. Mitigated by the unique index (one indexed row) and
  by `vault.Resolve` being the proxy's existing per-request step; a future
  optimization may cache resolved secrets per endpoint with a short TTL, but the
  authoritative expiry/revocation check stays on every call (fail-closed).
- No in-place renewal: long sessions must re-launch. Accepted for v1; a renew
  endpoint is named as future work.
- Boundary→`drun_config` compilation introduces a translation layer whose field
  mapping this ADR does not fully specify; the implementing batch owns it, with
  the constraint that it be deterministic for a stable `content_hash`.

## Alternatives considered

- **Proxy token as a stateless JWT** (like the ADR-0001 session token). Rejected:
  the proxy must read the grant's `endpoint_id`/`scopes`/expiry from the DB
  regardless, so a self-verifying token saves no round-trip and makes revocation
  a denylist problem instead of a single `revoked_at` stamp.
- **Store the raw proxy token** (or a reversible form). Rejected outright — the
  schema comment and threat model both require hash-only storage; a DB leak must
  not yield usable credentials.
- **Signing the boundary digest** (matching the OpenAPI `SigningMetadata`
  wording) and shipping only the boundary. Rejected: the drun engine enforces
  `drun_config`, not the boundary, and `pkg/policy` verifies a signature over the
  `drun_config`-derived `SigningInput`; signing only the boundary would leave the
  enforced config unauthenticated. We sign the `pkg/policy` bundle and name the
  contract follow-up instead.
- **Reusing the ADR-0001 JWT signing key for bundles.** Rejected: different
  purpose and lifecycle; a separate Ed25519 bundle key (own `kid`) limits blast
  radius and lets the two rotate independently.
- **`404` for the not-entitled case** (hiding the agent). Rejected: agents are
  already org-visible via `listAgents`, so `403` leaks nothing, and the contract
  documents `403` for exactly this. (Cross-*org* references still return `404`.)
- **Creating the session `pending` and flipping to `active` on first usage.**
  Rejected for v1: there is no activation callback in the contract and it adds a
  state with no consumer; `active`-on-launch is simpler and honest. `pending`
  stays reserved for a real async path.
- **Minting the grant and the session in separate calls (no transaction).**
  Rejected: it admits a window where a token exists without an enforceable
  session, or a session without credentials — both violate fail-closed.
- **Enforcing budgets inline in `launchSession`.** Deferred, not rejected on
  merit: metering attribution is fixed here (§5), but cap evaluation is a
  distinct concern with its own batch, kept out to keep this decision focused.

## Out of scope for this ADR (explicit)

- **Any OpenAPI or migration change.** The `PolicyBundle`/`SigningMetadata`
  reconciliation (§2) and a possible **renew** endpoint (§4) are **proposals**
  for later batches, not changes made here.
- **Signing-key management** — generation, storage, rotation, and `kid`
  distribution for both the bundle key and the ADR-0001 JWT key. Named, not
  specified (consistent with `pkg/policy` and ADR-0001).
- **The exact `PolicyBoundary → DrunConfig` field mapping** and the concrete
  `scopes` JSON schema — owned by the implementing batches; this ADR fixes their
  properties (deterministic compile; policy-derived, non-client-supplied
  scopes), not their field lists.
- **Budget / spend enforcement at the proxy** — a separate concern from the
  metering seam (§5).
- **Proxy-side secret caching / performance tuning** beyond the note that the
  expiry/revocation check stays per-call.
- **Environment (`environment_id`) resolution** and any environment-specific
  launch behavior — the column exists but its selection is not part of this
  flow's v1 decision.
- **CLI-side storage of the returned `proxy_token`** — governed by ADR-0001's
  `0600`-file model for session material.

## References

- `openapi/v1.yaml` — `launchSession`, `terminateSession`, `getSession`,
  `SessionLaunchRequest`, `SessionLaunchResponse`, `PolicyBundle`,
  `PolicyBoundary`, `SigningMetadata`, the inline `403` on launch.
- `migrations/000004_agent_catalog.up.sql`,
  `migrations/000005_runtime_metering.up.sql` — `agents`, `agent_versions`,
  `entitlements`, `policy_versions`, `sessions`, `credential_grants`,
  `usage_events`, `session_status`.
- `internal/store` — `SessionStore`, `CredentialGrantStore` (`GetByTokenHash`,
  `Revoke`), `UsageEventStore`, `EntitlementStore`, `AgentVersionStore`,
  `PolicyVersionStore`, `ModelEndpointStore`, `TeamMembershipStore`.
- `internal/vault` — `Vault.Resolve`, `Secret` (redacted).
- `pkg/policy` — `PolicyBundle`, `DrunConfig.ToTOML`, `SigningInput`, `Verify`
  (fail-closed), Ed25519. `docs/policy/DRUN_POLICY_BUNDLE.md` — bundle format.
- `internal/proxy` — the broker's vault-injection + streaming passthrough.
- [ADR-0001](0001-authentication-and-session-model.md) — Bearer JWT, principal
  in context, hashed-token revocation, fail-closed offline.
- [ADR-0002](0002-rest-api-conventions.md) — org scoping, `403`-vs-`404` rule,
  error mapping, naming contract↔schema gaps as follow-ups.
- `docs/security/threat-model.md` — credential custody, scoped short-lived
  tokens, proxy-bypass, prompt revocation.
- `docs/ARCHITECTURE.md`, `docs/TECH_STACK.md` — planes and the locked stack.
