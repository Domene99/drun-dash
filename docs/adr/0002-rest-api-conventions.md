# 0002 — REST API conventions (org scoping, error mapping, pagination, DTO mapping)

- **Status:** Proposed
- **Date:** 2026-07-11
- **Deciders:** drun-dash control-plane / handler work streams
- **Batch:** 6 (decision artifact; no code)

## Context

The next batch implements the control-plane REST handlers — agents, policies,
sessions, approvals, and analytics — on top of `internal/store`. Several agents
will write these handlers **in parallel**. Handlers share four cross-cutting
concerns that, if each handler decides on its own, will drift: how the caller's
organization is derived and enforced, how store and validation errors become
HTTP responses, how list endpoints paginate, and how the OpenAPI DTOs are
reconciled with the store models when the two disagree.

This ADR **decides those conventions once**, grounded in what already exists on
`main`, so the parallel handlers compose. It writes **no code** and changes
neither the OpenAPI contract nor the migrations. Where the contract and the
schema genuinely disagree, this ADR records the handler's behavior and, where a
follow-up contract change is warranted, names it explicitly as a proposal for a
later batch — it does not make that change here.

What already exists and constrains this decision:

- **API contract — `openapi/v1.yaml`** is the source of truth. Relevant shared
  pieces:
  - The `Error` schema is a **flat** object (`code`, `message`, optional
    `details`), used directly as the body of every error response. There is no
    outer `{"error": …}` wrapper.
  - The reusable responses are exactly **`BadRequest` (400)**, **`Unauthorized`
    (401)**, **`NotFound` (404)**, and **`NotImplemented` (501)** — all bodied
    by `Error`. **`403` and `409` are *not* reusable responses**; they appear
    only inline on two operations (`launchSession` → `403`, `decideApproval` →
    `409`), each still bodied by the same flat `Error` schema.
  - Cursor pagination: the `Page` schema contributes a single nullable
    `next_cursor`. The shared parameters are `PageLimit` (`limit`, integer,
    `minimum: 1`, `maximum: 200`, `default: 50`) and `PageCursor` (`cursor`,
    opaque string). Only five list operations carry both parameters and embed
    `Page`: `listAgents`, `listPolicies`, `listSessions`,
    `listTrajectoryEvents`, `listApprovals`. The nested lists `listAgentVersions`,
    `listAgentEntitlements`, `listPolicyVersions` and the analytics reads
    (`querySpend`, `listBudgets`) return a plain `{items:[…]}` with **no**
    `Page` envelope and **no** page parameters.
  - The global security scheme is `bearerAuth` (JWT, per ADR-0001). The
    resource paths (`/agents`, `/policies`, …) carry **no org path parameter** —
    org scope is implicit in the authenticated principal.
- **HTTP building blocks — `internal/httpx`.** `WriteJSON(w,status,v)`,
  `WriteError(w,status,code,message)`, `WriteErrorDetails(w,status,code,message,details)`,
  the flat `ErrorBody{Code,Message,Details}`, and the stable code constants
  `CodeBadRequest` (`"bad_request"`), `CodeUnauthorized` (`"unauthorized"`),
  `CodeForbidden` (`"forbidden"`), `CodeNotFound` (`"not_found"`), `CodeConflict`
  (`"conflict"`), `CodeInternal` (`"internal_error"`), `CodeNotImplemented`
  (`"not_implemented"`). `DecodeJSON` enforces a 1 MiB cap, rejects unknown
  fields, and rejects trailing data; its error is meant to map straight to a
  400. Handlers **must** use these helpers, not hand-rolled JSON.
- **Persistence — `internal/store`.** Lookups and mutations return the sentinels
  `ErrNotFound` (wrapping `pgx.ErrNoRows`) and `ErrConflict` (wrapping a
  unique-constraint violation, `23505`, with the constraint name). Callers
  branch with `errors.Is`. Models use UUID-as-string, `time.Time` for
  `TIMESTAMPTZ`, `json.RawMessage` for JSONB, and typed enum constants. The list
  methods are **org- or parent-scoped already** (e.g. `AgentStore.ListByOrg`,
  `EntitlementStore.ListByAgent`) and take **no `limit`/`cursor`** yet; their
  `ORDER BY` differs per table (see §3).
- **Auth model — ADR-0002's predecessor, ADR-0001.** A chi middleware validates
  the Bearer JWT and injects the resolved principal (member id, `org_id`, roles)
  into the request `context.Context`. Handlers read the principal from context;
  they never parse the token themselves.

## Decision drivers

- **Contract fidelity.** Emit exactly the shapes `openapi/v1.yaml` documents;
  reuse `httpx` so bodies stay conformant. Propose contract changes explicitly,
  never silently diverge.
- **Consistency across parallel handlers.** One rule per concern, expressed so
  that an agent writing the approvals handler and an agent writing the agents
  handler reach the same behavior without coordinating.
- **Tenant isolation is non-negotiable.** No handler may read or write another
  organization's rows. This is a security boundary, not a convenience.
- **Cheap, stable pagination.** Keyset (seek) pagination over an index-friendly
  ordering, with an opaque cursor the client cannot use to page across orgs or
  to inject a scan.
- **Reuse what `main` already provides.** Prefer the existing `httpx` codes and
  `store` sentinels over inventing parallel machinery.

## Decision

### 1. Org scoping — the caller's org comes from the token, and every store call is filtered by it

- **Source of truth for the org.** A handler determines the caller's
  organization **only** from the principal that the ADR-0001 authentication
  middleware injected into the request `context.Context` — i.e. the validated
  JWT's `org_id` claim. Handlers **never** accept an org from the path, query,
  body, or a header; the contract deliberately omits an org path parameter for
  exactly this reason. A request that reaches a protected handler without a
  principal in context is a programming error in the middleware chain, not a
  client error — the middleware already rejects unauthenticated requests with
  `401` before the handler runs.
- **Every store call is org-filtered.** The org id read from context is passed
  as the org argument to every store method that takes one
  (`ListByOrg(ctx, orgID)`, `Create(ctx, orgID, …)`, etc.). This is the primary
  isolation mechanism for **list** and **create**: the store's `WHERE org_id =
  $1` guarantees the result set never crosses tenants.
- **Get / update / delete by id: verify ownership, do not trust the id.** The
  by-id store lookups (`GetByID`) key on the primary key alone and are **not**
  org-scoped, so a caller could otherwise reference another org's UUID. The rule
  is: **after loading a row by id, the handler compares the row's `OrgID` (or,
  for nested resources, the owning parent's `OrgID`) to the caller's org; on
  mismatch it returns `404 not_found`, never `403`.** Returning `404` (not
  `403`) avoids leaking the existence of a resource in another tenant. The same
  applies to nested resources reached through a parent id (an agent's versions,
  entitlements): resolve the parent, confirm the parent belongs to the caller's
  org, then operate. Cross-org reads and cross-org writes are both forbidden;
  there is no "read is harmless" exception.
- **Why in the handler and not (only) the store.** The store's by-id methods are
  intentionally generic. Rather than fork every store signature, the ownership
  check lives in a small shared handler helper (owned by the implementing batch)
  so the rule is applied uniformly. Where a store already exposes an org-scoped
  path (e.g. `ListByOrg`), handlers use it in preference to load-then-check.

### 2. Error mapping — one canonical table from store/validation failures to HTTP + flat `Error`

Handlers map failures to responses through `httpx` helpers using the constants
below. This is the **complete** canonical mapping; handlers do not invent codes.

| Condition | HTTP | `httpx` code constant | code string | Emit with |
| --- | --- | --- | --- | --- |
| Request body/params malformed, unknown field, trailing data, over-limit body (`httpx.DecodeJSON` error) | `400` | `CodeBadRequest` | `bad_request` | `WriteError` / `WriteErrorDetails` |
| Semantic validation failure (missing required field, bad enum, out-of-range) | `400` | `CodeBadRequest` | `bad_request` | `WriteErrorDetails` with `details.field` |
| Missing/invalid/expired token | `401` | `CodeUnauthorized` | `unauthorized` | handled by ADR-0001 middleware, not the handler |
| Authenticated but not entitled (only where the contract defines `403`, e.g. `launchSession`) | `403` | `CodeForbidden` | `forbidden` | `WriteError` |
| `errors.Is(err, store.ErrNotFound)`, **or** a row loaded by id belongs to another org | `404` | `CodeNotFound` | `not_found` | `WriteError` |
| `errors.Is(err, store.ErrConflict)` (unique-constraint violation; only where the contract defines `409`, e.g. `decideApproval`, or on create collisions) | `409` | `CodeConflict` | `conflict` | `WriteError` |
| Operation defined by the contract but not yet built | `501` | `CodeNotImplemented` | `not_implemented` | `WriteError` |
| Any other error (store/query failure, encode failure, unexpected) | `500` | `CodeInternal` | `internal_error` | `WriteError`; log with the request id. Never leak the raw error to the client. |

Rules that make the table unambiguous:

- **Branch on sentinels with `errors.Is`**, never on error strings. `ErrConflict`
  is wrapped with the constraint name (`%w on <constraint>`), so `errors.Is`
  still matches; the constraint name may go into `details` for a create
  collision but must not change the code.
- **`403` and `409` only where the contract allows them.** Because they are not
  reusable responses, a handler emits `403`/`409` **only** on the two operations
  whose OpenAPI definition documents that status (`launchSession` → `403`,
  `decideApproval` → `409`), plus `409` on the create operations whose unique
  constraints make a conflict reachable (e.g. duplicate `(org_id, name)` on
  `createAgent` / `createPolicy`). If a store returns `ErrConflict` from an
  operation the contract does **not** document as `409`, that is a contract gap;
  the handler still returns `409 conflict` (the honest status) **and** the gap
  is logged as a follow-up to reconcile the contract — it must not be
  down-mapped to `400`/`500` to hide the mismatch.
- **`401` is the middleware's job.** By the time a handler runs, the caller is
  authenticated (ADR-0001). Handlers therefore never mint `401`; they mint
  `403` for entitlement failures where the contract allows it.
- **Validation `details`.** Field-level validation failures use
  `WriteErrorDetails` with a `details` object (e.g. `{"field":"name"}`),
  matching the `Error.details` free-form map. Decode-layer failures
  (`httpx.DecodeJSON`) carry the decoder's message as `message` and need no
  `details`.

### 3. Pagination — keyset cursor over `(created_at, id)`, base64url-encoded, bounded limit

Applies to the five paginated operations only (`listAgents`, `listPolicies`,
`listSessions`, `listTrajectoryEvents`, `listApprovals`). The nested lists and
analytics reads return all rows in their existing order with no cursor.

- **Ordering — `ORDER BY created_at, id`.** Keyset pagination requires a total,
  stable order over an immutable, indexed tuple. `created_at` alone is not
  unique, so it is tie-broken by the primary key `id`; the pair is unique and
  monotonic (rows are append-only at creation). This matches the ordering the
  stores already use for the append-only tables (`organizations`,
  `entitlements`, `budget_rules`, `routing_rules` all `ORDER BY created_at,
  id`). **Note the mismatch to reconcile:** `AgentStore.ListByOrg` and
  `PolicyStore.ListByOrg` currently `ORDER BY name` and take no `limit`/`cursor`.
  The keyset convention therefore requires the implementing batch to **add
  paginated list variants** (e.g. `ListByOrgPage(ctx, orgID, limit, cursor)`)
  that order by `created_at, id`; ordering user-facing lists by name is a
  presentation concern that can be layered later but is **not** the pagination
  key. Handlers must not attempt keyset pagination over a `name` ordering
  (names are mutable and non-unique — the cursor would be unstable).
- **Cursor encoding — opaque, but concretely `base64url(created_at RFC3339Nano
  + "\x00" + id)`.** `next_cursor` is the last returned row's `(created_at, id)`
  serialized as `<created_at>\x00<id>` and `base64url`-encoded (no padding). It
  is **opaque to clients** — they echo it back verbatim via the `cursor`
  parameter. It carries **no org id**: the org is always re-derived from the
  token (§1), so a cursor cannot be replayed to page another tenant, and a
  forged cursor can at worst select a starting point within the caller's own
  org.
- **The seek predicate.** Given a decoded cursor `(c_ts, c_id)`, the next page's
  query adds `WHERE (created_at, id) > ($cursor_ts, $cursor_id)` (row-value
  comparison, matching the `ORDER BY created_at, id`) and `LIMIT $limit + 1`.
  The extra row is a lookahead: if the query returns `limit + 1` rows, the
  handler drops the extra, sets `next_cursor` from the last **kept** row, and
  returns it; if it returns `≤ limit` rows, the page is the last one and
  `next_cursor` is `null` (the contract's nullable field).
- **Parsing and bounding `limit`.** `limit` is parsed from the query string; an
  absent value defaults to **50**, values are clamped to **`[1, 200]`** (the
  contract's `minimum`/`maximum`). A non-integer `limit` is a `400 bad_request`.
  An out-of-range integer is **clamped**, not rejected, so a client asking for
  `limit=1000` gets 200 rather than an error (friendlier, and the contract's
  `maximum` is the real ceiling).
- **Parsing `cursor`.** An absent `cursor` starts from the beginning. A present
  `cursor` that fails to `base64url`-decode, or whose decoded form is not a
  valid `(timestamp, id)` pair, is a `400 bad_request` (`"invalid cursor"`) —
  never a silent reset to the first page, which would loop clients forever.
- **Consistency guarantees.** Keyset pagination is immune to the "row inserted
  during paging shifts the offset" bug that afflicts `LIMIT/OFFSET`. Because the
  key is `(created_at, id)` and rows are append-only, concurrent inserts either
  fall before the cursor (already returned, unaffected) or after it (picked up
  on a later page). Rows deleted mid-scan simply do not appear; no page is
  skipped or duplicated.

### 4. DTO ↔ store translation — per-mismatch behavior

Handlers translate between the OpenAPI DTOs and the `store` models in the
handler layer (a small unexported `toAPIAgent(store.Agent) api.Agent` style
mapper per resource). UUIDs are already strings on both sides; `time.Time`
serializes to RFC 3339 via the DTO's `Timestamp`; `json.RawMessage` passes
through unchanged where a DTO field is free-form JSON. The known structural
mismatches are decided as follows.

**4a. `Agent` — `latest_version` maps, `updated_at` maps, `default_policy_id`
is omitted (no backing column).**

| OpenAPI `Agent` field | Source | Behavior |
| --- | --- | --- |
| `id`, `name`, `description`, `created_at`, `updated_at` | `store.Agent.{ID,Name,Description,CreatedAt,UpdatedAt}` | Direct map. `description` is `*string`; omit when nil. |
| `latest_version` | `store.Agent.CurrentVersion` | **Map** `CurrentVersion → latest_version`. The names differ but the meaning is identical (highest published version, `0` until the first lands). Emit `0` as-is; the field is optional in the DTO. |
| `default_policy_id` | — | **Omit.** The `agents` table has **no** `default_policy_id` column; there is nothing to populate it from. Because the field is optional in `Agent` (not in `required`), omitting it is contract-valid. On **write** (`AgentCreate`/`AgentUpdate`), a supplied `default_policy_id` is **accepted and ignored** in v1 (documented, not silently dropped without record) — the closest existing concept is the per-version `policy_version_id` pin on `agent_versions`, which is set through `publishAgentVersion`, not through the agent row. **Follow-up (contract/schema):** decide whether `default_policy_id` becomes a real column or is removed from the contract; tracked for a later batch, not resolved here. |

**4b. `AgentVersion` — `manifest_digest`/`notes` have no backing columns; map
what exists, flag the gap.**

The `agent_versions` table stores `system_prompt`, `allowed_tools`,
`policy_version_id`, `endpoint_id`, `budget_id`, `fan_out_ceiling` — an
*expanded* manifest — whereas the DTO exposes an opaque `manifest_digest` plus
`notes`. There is no digest column and no notes column.

- **`agent_id`, `version`, `created_at`** map directly (all three are the DTO's
  `required` fields, so a valid `AgentVersion` can always be produced).
- **`manifest_digest`** has no stored value. Two honest options: (a) omit it —
  but the field, while `required` in `AgentVersionCreate`, is also `required` in
  the response `AgentVersion`, so omitting it makes the response
  **contract-invalid**; (b) **compute** a stable digest at read time from the
  stored manifest fields (`system_prompt`, `allowed_tools`, the pinned ids,
  `fan_out_ceiling`) — e.g. a SHA-256 over their canonical serialization.
  **Decision: compute** the digest deterministically from the stored fields so
  the required field is always populated and stable for identical content. On
  **write** (`publishAgentVersion` with `AgentVersionCreate.manifest_digest`),
  v1 cannot expand a digest back into the manifest columns, so the create path
  is a **known contract↔schema gap**: publishing a version needs the manifest
  fields the store requires, which `AgentVersionCreate` does not carry. The
  implementing batch returns `501 not_implemented` for `publishAgentVersion`
  until the contract gains the manifest fields (or the digest is defined as the
  server-computed value of a separately-supplied manifest). **Follow-up:**
  reconcile `AgentVersionCreate` with the `agent_versions` columns.
- **`notes`** has no column. **Omit** on read (optional field); on write,
  **accept and ignore** in v1, documented, pending the same follow-up.

**4c. `Entitlement` — translate the principal representation both directions.**

The DTO uses `principal_type ∈ {user, group}` + `principal_id`; the table uses
a `member_id` **XOR** `team_id` nullable pair. These are two encodings of the
same fact and map losslessly:

| Direction | Rule |
| --- | --- |
| store → DTO (read) | If `MemberID != nil` → `principal_type = "user"`, `principal_id = *MemberID`. If `TeamID != nil` → `principal_type = "group"`, `principal_id = *TeamID`. Exactly one is non-nil (the table's XOR), so the mapping is total and unambiguous. |
| DTO → store (write) | `principal_type = "user"` → set `member_id = principal_id`, `team_id = NULL`. `principal_type = "group"` → set `team_id = principal_id`, `member_id = NULL`. |
| `policy_id` | The DTO's `policy_id` corresponds to the store's `PolicyVersionID` pin (`*string`); map through, omit when nil. Note the DTO says *policy* while the store pins a *policy version* — the handler maps to whichever the store column is (`policy_version_id`); a follow-up may rename the DTO field for precision. |
| `max_fan_out` | Maps directly to `store.Entitlement.MaxFanOut` (`*int`); omit when nil. |

The vocabulary difference (`user`/`group` vs `member`/`team`) is purely
terminological and is **absorbed in the mapper** — no contract change needed;
it is the one mismatch that reconciles cleanly with zero data loss.

**General mapping rules.**

- **Nullable → omit.** A `*T` store field that is nil maps to an **absent** JSON
  property (the DTO fields are optional unless listed in `required`); handlers
  do not emit explicit `null` for optional scalars.
- **Never fabricate `required` data.** Where a DTO `required` field has no
  faithful source (`AgentVersion.manifest_digest`), the decision is to **compute
  a deterministic value** (§4b) rather than emit a placeholder or an invalid
  body. Where an *optional* field has no source (`Agent.default_policy_id`), the
  decision is to **omit**.
- **Ignored-on-write fields are documented, not silent.** Every field a write
  path accepts-and-ignores in v1 (`default_policy_id`, `notes`) is recorded here
  and carries a follow-up; handlers do not reject the field (staying
  forward-compatible with the contract) but do not silently pretend it was
  stored.

## Consequences

**Positive**

- The parallel batch-7 handlers share one org-scoping rule, one error table, one
  pagination scheme, and one DTO-mapping decision per resource — they compose
  without re-litigating any of it.
- Fully consistent with `openapi/v1.yaml` and `internal/httpx`: error bodies are
  the flat `Error`, codes are the existing `httpx.Code…` constants, and the
  paginated responses use the contract's `Page`/`PageLimit`/`PageCursor`.
- Tenant isolation is explicit and uniform: org from the token, filtered on
  every store call, `404` (not `403`) on cross-org id references.
- Keyset pagination is stable under concurrent inserts and needs no `OFFSET`
  scan; the cursor is opaque and org-safe.

**Negative / trade-offs**

- The `store` by-id methods are not org-scoped, so isolation for get/update/
  delete depends on a handler-layer ownership check. This is a discipline that
  must be applied on every by-id path; the shared helper mitigates it, but a
  handler that forgets it is an isolation bug. (A future hardening is org-scoped
  store methods.)
- Two DTOs cannot be served faithfully from the current schema:
  `AgentVersion.manifest_digest` is **computed** at read time, and
  `publishAgentVersion` is **`501`** until the contract carries the manifest
  fields. `Agent.default_policy_id` is **omitted** on read and **ignored** on
  write. These are recorded as follow-ups; they are contract↔schema debts, not
  handler bugs.
- Ordering user-facing lists by `created_at, id` (for a stable cursor) differs
  from the human-friendlier `name` ordering the non-paginated stores use;
  reconciling "sortable by name" with keyset paging is deferred.

## Alternatives considered

- **`LIMIT`/`OFFSET` pagination.** Simpler to implement over the existing
  `name`-ordered lists, but offsets shift when rows are inserted/deleted mid-scan
  (skipped or duplicated rows) and deep offsets scan-and-discard. Rejected in
  favor of keyset, which the contract's opaque `next_cursor` already anticipates.
- **Cursor = row `id` only.** Would require a unique, monotonic id ordering;
  UUIDs are not monotonic, so paging by id alone does not match creation order.
  `(created_at, id)` gives a meaningful, index-friendly order with a unique
  tie-break. Rejected.
- **Embedding the org id in the cursor.** Unnecessary and mildly risky — the org
  is always re-derived from the token, so putting it in the cursor adds a value
  to validate for no benefit and a replay-shaped footgun. Rejected; the cursor
  holds only `(created_at, id)`.
- **`403` instead of `404` for cross-org id references.** Leaks the existence of
  another tenant's resource. Rejected; cross-org references are indistinguishable
  from "does not exist" to the caller.
- **Adding org-scoped variants to every store `GetByID` now.** Cleaner isolation,
  but it is store surface owned by earlier batches and out of scope for a
  docs-only decision; the handler-layer check is the v1 answer, with store-level
  scoping noted as a future hardening. Rejected for now.
- **Emitting a placeholder `manifest_digest` (empty string / fixed value).**
  Satisfies the `required` field but lies about content identity and breaks
  digest-based caching/comparison. Rejected in favor of a deterministic computed
  digest.
- **Down-mapping an unexpected `ErrConflict` to `400`/`500`.** Hides a
  contract↔schema gap behind a wrong status. Rejected; the handler returns the
  honest `409` and the gap is logged for contract reconciliation.

## Out of scope for this ADR (explicit)

- **Any OpenAPI or migration change.** The follow-ups this ADR names
  (`default_policy_id` column vs contract removal, `AgentVersionCreate` manifest
  fields, `Entitlement.policy_id` rename) are **proposals** for later batches,
  not changes made here.
- **The concrete `api` DTO Go types.** Whether they are hand-written or generated
  from `openapi/v1.yaml`, and their package location, is the implementing batch's
  call; this ADR only fixes the field-by-field mapping behavior.
- **Authentication and the principal shape.** Owned by
  [ADR-0001](0001-authentication-and-session-model.md); this ADR consumes the
  injected principal and does not redefine it.
- **Sorting/filtering query parameters** beyond `limit`/`cursor` (e.g. sort by
  name, filter by status) — not in the contract's list operations for v1.
- **Rate limiting, idempotency keys, and conditional requests (ETags)** — not
  part of these conventions.
- **Analytics query shaping** (`querySpend`, `listBudgets` windowing) beyond the
  note that they are unpaginated `{items:[…]}` reads.

## References

- `openapi/v1.yaml` — `Error`, `Page`, `PageLimit`, `PageCursor`, reusable
  `BadRequest`/`Unauthorized`/`NotFound`/`NotImplemented` responses, inline
  `403`/`409` on `launchSession`/`decideApproval`, `Agent*`, `AgentVersion*`,
  `Entitlement*`, `Policy*` schemas, `bearerAuth`.
- `internal/httpx/json.go` — `WriteJSON`, `WriteError`, `WriteErrorDetails`,
  `ErrorBody`, the `Code…` constants, `DecodeJSON`.
- `internal/store` — `ErrNotFound`, `ErrConflict`, `mapError`; `Agent`,
  `AgentVersion`, `Entitlement`, `Policy` models; the `ListBy…` methods and
  their `ORDER BY` clauses.
- [ADR-0001](0001-authentication-and-session-model.md) — authentication and the
  request principal injected into `context.Context`.
- `docs/ARCHITECTURE.md`, `docs/TECH_STACK.md` — planes and the locked stack.
