# 0006 — RBAC and access model (roles, custom roles, permissions)

- **Status:** Proposed
- **Date:** 2026-07-15
- **Deciders:** drun-dash control-plane / admin-console / CLI work streams
- **Batch:** 21 (decision artifact; no code)

## Context

Access on `main` today is **coarse and split across two mechanisms that never
meet**:

- **`members.role`** is a single `member_role` enum
  (`migrations/000002_identity.up.sql`): `admin | member | auditor`, one value
  per member, defaulting to `member`. It travels into the session JWT as the
  `roles` claim (`internal/auth/token.go`, `internal/auth/README.md`) — "coarse
  `member_role`s".
- **`entitlements`** (`migrations/000004_agent_catalog.up.sql`) are raw rows
  granting **one agent** to **one principal** (a member **XOR** a team), with an
  optional `policy_version_id` override and optional `max_fan_out`. They decide
  *launch* authorization and nothing else, and there is no management UI.

Two gaps follow. First, **nobody enforces the enum.** A grep of the mutating
handlers (`internal/httpapi/agents.go`, `policies.go`, `approvals.go`,
`analytics.go`) finds **no role or permission check** anywhere: `createAgent`,
`updateAgent`, `publishAgentVersion`, `createPolicy`, `publishPolicyVersion`,
`setAgentEntitlements`, `decideApproval`, and `querySpend` are gated by
[ADR-0001](0001-authentication-and-session-model.md)/`authmw.RequireAuth`
(authentication) and by org scoping
([ADR-0002](0002-rest-api-conventions.md)), but **any authenticated member of
the org can call them**. The `member_role` enum is decorative. Second,
**agent access is ungoverned and unmanaged**: it lives only in `entitlements`
rows, with no notion of a reusable bundle of "these permissions plus these
agents" that an admin can name, assign, and reuse.

The owner wants **role-based access**: assign **roles** to members (Owner,
Admin, Auditor, Member, plus **custom** roles an admin creates), where a custom
role grants **both** the permissions **and** the specific agents needed for a
task, all managed in the admin UI. The high-level model — role-based access — is
**approved**; this ADR locks the **model, schema, API, and enforcement** so the
migration, the store, the `RequirePermission` middleware, the roles/assignment
handlers, and the admin console are built against one design instead of five.
It writes **no code** and changes neither `openapi/v1.yaml` nor the migrations;
where the target model and the code on `main` diverge (notably the launch
authorization in `internal/httpapi/launch.go` and the missing per-endpoint
authorization), it records the change and names the follow-up, exactly as
[ADR-0003](0003-session-launch-and-credential-grant.md) does.

What already exists and constrains this decision:

- **Identity — `migrations/000002`.** `members` (`org_id`, `idp_subject` unique
  per org, `email`, `role member_role DEFAULT 'member'`, `active`); `teams`
  (`org_id`, `name` unique per org); `team_memberships` (composite-PK
  `team_id`+`member_id` join). `member_role` enum = `admin|member|auditor`.
- **Catalog / access — `migrations/000004`.** `agents`, `agent_versions`
  (pins `policy_version_id`, `endpoint_id`, `budget_id`, `fan_out_ceiling`),
  `policies`/`policy_versions`, `budgets`/`budget_rules`, and `entitlements`
  (agent → member XOR team; optional `policy_version_id` override + `max_fan_out`;
  unique per `(agent, member)` / `(agent, team)`).
- **Launch authorization — `internal/httpapi/launch.go` §1
  (per ADR-0003).** `launchSession` decides who may run an agent by intersecting
  `EntitlementStore.ListByAgent` with the caller's `{member_id} ∪ team_ids`
  (`resolveEntitlement`); no match → the one contract-sanctioned **`403`**. The
  matched entitlement's `policy_version_id` override and `max_fan_out` then feed
  the effective policy (§1.4) and the fan-out clamp (`clampFanOut`, §1).
- **Auth principal — ADR-0001 / `internal/auth` / `internal/authmw`.** The
  session JWT carries `{Subject, OrgID, MemberID, Roles}`; `authmw.RequireAuth`
  verifies it and stashes `*auth.Claims` in the request context;
  `authmw.MustPrincipal(ctx)` reads it. Handlers derive the org **only** from the
  principal. `Roles` is documented as the coarse `member_role`s.
- **API contract — `openapi/v1.yaml`.** The mutating operations that need
  authorization: `createAgent`, `updateAgent`, `deleteAgent`,
  `publishAgentVersion`, `setAgentEntitlements`; `createPolicy`,
  `publishPolicyVersion`; `launchSession`, `terminateSession`; `createApproval`,
  `decideApproval`; `querySpend`, `ingestTrajectoryEvents`. Read operations
  (`listAgents`, `getAgent`, `listPolicies`, `listSessions`, `listApprovals`,
  `listBudgets`, `getCurrentIdentity`, …) are org-scoped reads. There are **no**
  member-management, endpoint-management, budget-write, or role endpoints yet.
- **ADR-0002 — error mapping.** Cross-org / non-existent id references return
  **`404`** (never `403`); `403`/`409` are emitted **only** where the contract
  documents them. Where contract and code diverge, record the behavior and name
  the contract follow-up rather than silently diverge.

## Decision drivers

- **Enforcement is the product change, not the schema.** The owner's real ask is
  that mutating endpoints stop trusting "authenticated" and start checking "may
  *this* principal do *this*". The tables exist to answer that question cheaply
  and correctly; the middleware is where the value lands.
- **One access primitive.** Permissions ("what may I do") and agent access
  ("which agents may I run") should hang off **one** object — a role — so an
  admin names it once and assigns it, instead of juggling an enum plus loose
  `entitlements` rows.
- **Union / least-astonishment.** A member with several roles gets the **union**
  of their grants; adding a role only ever grants, and removing one only ever
  revokes. No role subtracts from another.
- **Prompt revocation, fail-closed.** Removing a role or permission must take
  effect on the **next request**, so authorization is resolved from the database
  per request, never trusted from a long-lived token claim — the same
  fail-closed, prompt-revocation posture ADR-0001/ADR-0003 hold for credentials.
- **Lossless migration.** The existing `member_role` values and every
  `entitlements` row must map onto the new model with no loss of access and no
  loss of the per-grant `policy_version_id`/`max_fan_out` tuning, and be
  reversible.
- **Contract fidelity, honest gaps.** New `403`s and new endpoints are named as
  OpenAPI follow-ups (the ADR-0002 rule), not silently shipped.
- **Reuse `main`.** Model roles as org-scoped rows with normalized join tables,
  exactly like `teams`/`team_memberships`/`entitlements`, and resolve them
  through the existing store + `authmw` principal rather than new machinery.

## Decision

### 1. Roles are named, org-scoped objects

A **role** is an org-scoped, named object bundling **(a)** a set of permissions
and **(b)** agent access. Every org is seeded with four **built-in** roles at
creation (and on migration), and admins may create any number of **custom**
roles:

| Role | Permissions | Agent access |
|------|-------------|--------------|
| **Owner** (super-admin) | **every** permission | **all agents** |
| **Admin** | all `*.manage` + `approvals.decide` + `audit.view` (not `agents.run`) | none by default |
| **Auditor** | `audit.view` | none |
| **Member** | `agents.run` | none by default (assigned per member/team) |
| **Custom** (admin-defined) | any subset of the catalog | a chosen agent set, or "all agents" |

Built-in roles are marked `builtin = true`: their **permission set and name are
immutable** (you cannot strip `roles.manage` from Owner or rename Admin) and
they **cannot be deleted**; their *agent access* and *assignments* are still
editable (e.g. grant the built-in Member role a default agent). Custom roles are
fully editable and deletable. "Owner ≠ Admin" is deliberate: Admin *manages* the
catalog but does not *run* agents unless separately granted a run role, keeping
management and execution as least-privilege-separable concerns. An org always
has at least one Owner assignment (see §5 backfill and §7).

### 2. Permission catalog

Permissions are a **fixed, app-level catalog** of coarse-grained verbs — one per
governable capability, not per endpoint:

| Permission | Governs |
|------------|---------|
| `agents.manage` | create/update/delete agents, publish agent versions, manage an agent's role access |
| `policies.manage` | create policies, publish policy versions |
| `endpoints.manage` | manage model endpoints *(future surface)* |
| `budgets.manage` | manage budgets and budget rules *(future surface)* |
| `members.manage` | manage members; assign roles to members and teams |
| `roles.manage` | create/update/delete roles; define their permissions and agent access |
| `approvals.decide` | approve / deny escalation requests |
| `audit.view` | view org-wide audit log, spend analytics, and all sessions/trajectories |
| `agents.run` | launch sessions — for the agents the granting role also grants (§3) |

The catalog is **validated at the application layer**, not as a PostgreSQL
`ENUM`. Justification: the list will grow (`endpoints.manage` and
`budgets.manage` already anticipate surfaces that do not exist yet), and a PG
enum requires an `ALTER TYPE` migration for every addition, whereas a `TEXT`
column checked against a Go-side catalog ships a new permission as a code change
plus a seed update — no type migration, no lock. Unknown permission strings are
rejected on write and ignored (fail-closed) on read.

`agents.run` is special: it is **necessary but not sufficient** to launch an
agent. Holding `agents.run` says "this principal may run agents *in general*";
**which** agents is decided by the role's agent access (§3). The two are
evaluated together in §6.

### 3. A role grants two things: permissions and agent access

Agent access is expressed on the role as **either** an explicit set of agents
**or** an "all agents" flag:

- `roles.all_agents BOOLEAN NOT NULL DEFAULT false` — when `true`, the role
  grants access to **every agent in the org** (present and future); the explicit
  set is then ignored. Owner has `all_agents = true`.
- `role_agents(role_id, agent_id)` — the explicit set, a composite-PK join, one
  row per granted agent, used when `all_agents = false`.

To migrate `entitlements` **losslessly** (§5), `role_agents` also carries the two
optional per-grant tuning columns entitlements offered:
`policy_version_id UUID` (a policy-version override for this agent) and
`max_fan_out INTEGER`. These preserve today's launch behavior: after the
migration, `launch.go`'s effective-policy pick (ADR-0003 §1.4) and `clampFanOut`
(§1) read the override/ceiling from the resolved `role_agents` grant instead of
the entitlement. `all_agents` grants carry no per-agent tuning (they fall back to
the agent version's own pins), which matches Owner/"all agents" semantics.

### 4. Members get one or more roles; effective access is the union

Roles reach a member two ways, mirroring how `entitlements` and budgets already
scope to a member **or** a team:

- `member_roles(member_id, role_id)` — a role assigned **directly** to a member.
- `team_roles(team_id, role_id)` — a role assigned to a **team**; every member of
  that team (via `team_memberships`) inherits it.

A member's **effective grants = the union across every role reachable directly
(`member_roles`) or via a team (`team_roles` ∘ `team_memberships`)**:

- **Effective permissions** = the union of every role's permission set.
- **Effective agent access** = "all agents" if **any** effective role has
  `all_agents = true`, otherwise the union of every effective role's
  `role_agents`.
- **A member may launch agent A** iff they hold `agents.run` **and** A is in
  their effective agent access (§6). When the same agent is granted by multiple
  `role_agents` rows, the **most permissive** tuning wins — effective
  `max_fan_out` is the **maximum** across grants (union = more access; the
  agent-version and policy ceilings still clamp it down in `clampFanOut`); a
  per-agent `policy_version_id` override applies only when the reachable grants
  agree on it, otherwise it is dropped and the agent version's own pin stands
  (deterministic and safe).

Union semantics make assignment monotonic: granting a role can only add access,
revoking one can only remove it, and no role ever subtracts from another.

### 5. Schema, reconciliation, and migration

A **new migration** (implemented later — this ADR fixes the shape) adds:

```
roles(
  id            UUID PK,
  org_id        UUID NOT NULL REFERENCES organizations ON DELETE CASCADE,
  name          TEXT NOT NULL,
  description   TEXT,
  builtin       BOOLEAN NOT NULL DEFAULT false,
  all_agents    BOOLEAN NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
)
role_permissions(
  role_id     UUID NOT NULL REFERENCES roles ON DELETE CASCADE,
  permission  TEXT NOT NULL,              -- validated against the app catalog (§2)
  PRIMARY KEY (role_id, permission)
)
role_agents(
  role_id           UUID NOT NULL REFERENCES roles ON DELETE CASCADE,
  agent_id          UUID NOT NULL REFERENCES agents ON DELETE CASCADE,
  policy_version_id UUID REFERENCES policy_versions ON DELETE SET NULL,  -- optional override
  max_fan_out       INTEGER,                                            -- optional, CHECK >= 0
  PRIMARY KEY (role_id, agent_id)
)
member_roles(
  member_id UUID NOT NULL REFERENCES members ON DELETE CASCADE,
  role_id   UUID NOT NULL REFERENCES roles   ON DELETE CASCADE,
  PRIMARY KEY (member_id, role_id)
)
team_roles(
  team_id UUID NOT NULL REFERENCES teams ON DELETE CASCADE,
  role_id UUID NOT NULL REFERENCES roles ON DELETE CASCADE,
  PRIMARY KEY (team_id, role_id)
)
```

Plus supporting indexes: `roles(org_id)`; `role_permissions(permission)` (the
`RequirePermission` lookup, §6); `member_roles(role_id)`, `team_roles(role_id)`,
`role_agents(agent_id)` for reverse lookups ("who holds this role", "who may run
this agent").

**Permissions as a table, not a `jsonb`/`text[]` column.** A normalized
`role_permissions` matches the codebase's relational grain
(`team_memberships`, `entitlements`) and, crucially, lets enforcement (§6) ask
"does any of this member's roles grant permission P" as a single indexed
`EXISTS` join — `member_roles`/`team_roles` ⋈ `role_permissions WHERE permission
= P` — rather than loading every role and scanning a JSON array in application
code. It also gives a clean `ON DELETE CASCADE` and a natural composite PK that
dedupes automatically. The cost — a handful of rows per role instead of one
column — is negligible for a fixed ~9-entry catalog.

**Reconciling `members.role`.** The `member_role` enum column is **deprecated**:
after the migration it is **no longer read for authorization** (the source of
truth becomes `member_roles`/`team_roles`). It is **retained, not dropped**, for
one release so the backfill is reversible and any lingering reader degrades to
identity metadata rather than breaking; a later migration drops it once nothing
reads it. Correspondingly, the JWT `roles` claim (ADR-0001) is **no longer an
authorization input** — enforcement resolves roles from the database per request
(§6) so a revoked role takes effect immediately without reissuing tokens. The
claim may be retained as advisory identity context or later repurposed to carry
role ids, but a permission is **never** trusted from the token. This is called
out as an ADR-0001 follow-up.

**Reconciling `entitlements` — roles replace them for launch authorization.**
Launch authorization moves to roles (§6); `entitlements` is **deprecated** as an
authorization mechanism and its data is **migrated**, not abandoned. Retain the
table for one release for rollback, then drop it.

**Backfill (seed + map + convert), all in the migration:**

1. **Seed** the four built-in roles per org: Owner (`all_agents = true`, every
   permission), Admin, Auditor, Member — with the permission sets of §1.
2. **Map `member_role`:** insert `member_roles` rows — every `role = 'admin'`
   member → Admin; `'member'` → Member; `'auditor'` → Auditor. Assign **Owner**
   to the org's founding admin (the earliest-created `role = 'admin'` member per
   org); if an org has no admin, leave Owner unassigned and **log it** for manual
   assignment (never silently leave an org without an owner without a record).
3. **Convert `entitlements`:** for each **member** entitlement, ensure the
   member holds `agents.run` and gains the agent — realized as a per-member
   custom role `builtin = false` (e.g. named `"Direct grants — <member>"`)
   carrying `agents.run` and a `role_agents` row per granted agent (copying
   `policy_version_id`/`max_fan_out`), assigned via `member_roles`. For each
   **team** entitlement, the symmetric per-team custom role assigned via
   `team_roles`. This preserves the member-XOR-team split and every per-grant
   override losslessly, and — because effective access is a union (§4) — composes
   with the built-in Member role rather than conflicting with it.

The `down` migration drops the new tables and restores `entitlements`/
`members.role` as the authorization source.

### 6. Authorization enforcement (the real backend change)

The control plane gains a **permission gate** that mutating endpoints pass
through, turning today's authenticate-only handlers into authenticate-**and**-
authorize. Two forms:

- **`RequirePermission("agents.manage")`** — an `httpx.Middleware` (composing
  with `authmw.RequireAuth`, which runs first). It reads the principal
  (`authmw.MustPrincipal`), resolves the caller's **effective permission set**
  from the database (`member_roles` ∪ `team_roles` ⋈ `role_permissions`, §4/§5),
  and calls `next` iff the required permission is present. Otherwise it writes
  **`403 forbidden`** (`httpx.CodeForbidden`). Resolution is per request
  (optionally a short-TTL per-request/principal cache) so revocation is prompt;
  it is **never** taken from the JWT claim.
- **An agent-scoped run check** for `launchSession`, where the required
  permission (`agents.run`) is inseparable from the **specific agent** in the
  request body and so cannot be a static route middleware. A helper —
  `authorizeRun(ctx, agentID) (grant, ok)` — replaces `launch.go`'s
  `resolveEntitlement`.

**`launchSession` authorization changes (`internal/httpapi/launch.go` §1).**
Today `resolveEntitlement` intersects `EntitlementStore.ListByAgent` with the
caller's `{member_id} ∪ team_ids`. It is replaced by: **"does any of the
caller's effective roles grant `agents.run` *and* include this agent (via
`role_agents` or `all_agents`)?"** — i.e. the caller holds `agents.run` in their
effective permission set **and** the agent is in their effective agent access
(§4). No match → the same one contract-sanctioned **`403`** ("not entitled to
launch this agent"). The resolved `role_agents` grant supplies the
`policy_version_id` override and `max_fan_out` that the matched entitlement used
to supply, so ADR-0003 §1.4 (effective policy) and `clampFanOut` (§1) are
unchanged in shape — only their **source** moves from `entitlements` to
`role_agents`. **This ADR amends ADR-0003 §1.3** accordingly (see References).

**Endpoint → required permission** (existing OpenAPI operations):

| Operation | Required |
|-----------|----------|
| `createAgent`, `updateAgent`, `deleteAgent`, `publishAgentVersion` | `agents.manage` |
| `setAgentEntitlements` *(legacy; superseded by role-agent management)* | `roles.manage` |
| `createPolicy`, `publishPolicyVersion` | `policies.manage` |
| `launchSession` | `agents.run` **+ agent in effective access** (agent-scoped, above) |
| `terminateSession` | the session's launcher, or `members.manage` |
| `createApproval` | member-visible (requesting review is a member action) |
| `decideApproval` | `approvals.decide` |
| `querySpend` | `audit.view` |
| `ingestTrajectoryEvents` | authorized by the **session credential grant** (ADR-0003), not a member permission |
| roles/assignment endpoints (§7) | `roles.manage` (define) / `members.manage` (assign) |
| `endpoints.manage` / `budgets.manage` surfaces *(future)* | the matching permission |

**Read endpoints stay member-visible within the org.** `listAgents`, `getAgent`,
`listPolicies`, `getPolicy`, `listSessions`, `getSession`, `listAgentVersions`,
`listPolicyVersions`, `listApprovals`, `listAgentEntitlements`, `listBudgets`,
`getCurrentIdentity` require only org membership — no new permission — preserving
ADR-0002's "the org's catalog is visible to its members" posture. Broad
audit/analytics reads that expose **other members'** activity (`querySpend`, and
any future org-wide audit log) require `audit.view`; a member reading **their
own** sessions/trajectories stays member-visible.

**`403` posture and contract follow-up.** Returning `403` for a permission miss
leaks nothing new: the resource is in the caller's **own** org and already
visible via the list endpoints, so "this exists but you may not change it"
reveals no secret — the identical reasoning ADR-0003 §1 uses to justify its
launch `403` over a `404`. Cross-org / non-existent ids still return **`404`**
(ADR-0002 unchanged). But these mutating operations do **not yet document a
`403`** in `openapi/v1.yaml`; adding the `403` response to each is a named
**contract follow-up**, the same posture ADR-0002/ADR-0003 take for
contract↔behavior gaps.

### 7. API surface (new — proposed, to be implemented later)

Role and assignment management, all requiring the permissions from §6 and all
org-scoped per ADR-0002 (a cross-org `roleId` is a `404`):

- **Roles CRUD** — `GET /roles` (list), `POST /roles` (create custom),
  `GET /roles/{roleId}`, `PATCH /roles/{roleId}`, `DELETE /roles/{roleId}`. The
  role representation carries `name`, `description`, `builtin`, `all_agents`,
  its **permissions** array, and its **agents** array (with optional per-agent
  `policy_version_id`/`max_fan_out`); `PATCH` replaces the permission and agent
  sets wholesale, mirroring `setAgentEntitlements`' replace semantics.
  Mutating/deleting a `builtin` role's permissions or name → **`409 conflict`**;
  deleting a role still referenced by an assignment is likewise a `409` (or a
  documented cascade). `POST`/`PATCH`/`DELETE` require `roles.manage`.
- **Member/team assignment** — `PUT /members/{memberId}/roles` and
  `PUT /teams/{teamId}/roles` (replace the assigned-role set), requiring
  `members.manage`. `GET` variants list current assignments. Splitting the
  verbs — `roles.manage` **defines** what a role is, `members.manage` **assigns**
  it — keeps "who can invent capabilities" separate from "who can hand them out".
- **Guardrail:** the last Owner assignment in an org cannot be removed (a
  `409`), so an org can never be locked out of administration.

These operations, the `member_roles`/`team_roles`/roles schemas, and the new
`403`s are **OpenAPI additions** — named here, added by the implementing batch,
not made in this ADR.

## Consequences

**Positive**

- Mutating endpoints stop trusting "authenticated": one `RequirePermission`
  gate + one agent-scoped run check enforce least privilege across the whole
  control plane, closing the gap where any member could call `createAgent` or
  `decideApproval`.
- Permissions and agent access live on **one** named, reusable object; an admin
  builds a "run these three agents, view audit" custom role once and assigns it,
  instead of scattering `entitlements` rows and leaning on a decorative enum.
- Union semantics make assignment monotonic and predictable; direct- and
  team-scoped roles compose without special cases.
- Revocation is prompt and fail-closed: authorization is resolved from the DB per
  request, so removing a role takes effect on the next call without reissuing
  tokens — consistent with ADR-0001/ADR-0003.
- The migration is lossless and reversible: `member_role` values and every
  `entitlements` row (including `policy_version_id`/`max_fan_out`) map onto roles,
  and a `down` restores the old authorization source.
- Reuses `main`'s grain — org-scoped rows, composite-PK joins, the `authmw`
  principal, `httpx.Middleware`, and `launch.go`'s existing clamp/override logic
  (only its data source moves).

**Negative / trade-offs**

- **New authorization surface = new `403`s the contract does not yet document.**
  Carried as an explicit OpenAPI follow-up (§6), a debt, not a handler bug — the
  ADR-0002 posture.
- **Amends ADR-0003 §1.3:** launch authorization moves from the `entitlements`
  intersection to the role-based `agents.run` + agent-access check. Two access
  mechanisms coexist during the transition release (roles authoritative,
  `entitlements` retained for rollback) before `entitlements` is dropped.
- **A per-request permission resolution** adds a small indexed query to every
  mutating call. Mitigated by the `role_permissions(permission)` index and the
  `EXISTS`-join shape; a short-TTL per-request cache is available, but the
  authoritative check stays per request (prompt revocation).
- **Backfill of "all agents" custom roles** for members with many entitlements
  creates one custom role per such principal; acceptable, and cleaner than
  losing the per-grant overrides.
- **Owner assignment on migration is best-effort** (earliest admin); orgs with no
  admin need a logged manual step. Named, not hidden.

## Alternatives considered

- **Keep the `member_role` enum, just enforce it.** Rejected: an enum gives one
  role per member and no agent-scoping, so it cannot express "run these three
  agents plus view audit," which is the owner's requirement. Roles subsume the
  enum (the four built-ins) while adding custom roles and agent access.
- **Permissions as a `jsonb`/`text[]` column on `roles`.** Rejected: it forces
  enforcement to load every role and array-scan in app code, loses the clean
  `EXISTS`-join and composite-PK dedupe, and doesn't match the codebase's
  normalized joins. The row count is trivial for a fixed catalog.
- **A PostgreSQL `ENUM` for permissions.** Rejected: the catalog will grow
  (`endpoints.manage`, `budgets.manage` already anticipate future surfaces), and
  every addition would need an `ALTER TYPE` migration. A `TEXT` column validated
  against an app-level catalog ships a new permission as a code change.
- **Keep `entitlements` as the launch authority; layer roles only for
  permissions.** Rejected: it perpetuates two access mechanisms permanently and
  keeps agent access outside the manageable role object — the exact split the
  owner is trying to remove. Roles must own agent access; `entitlements` is
  migrated into `role_agents`.
- **Trust the JWT `roles` claim for authorization.** Rejected: tokens outlive
  role changes, so a revoked role would keep working until the token expired,
  breaking prompt revocation. Authorization is resolved from the DB per request;
  the claim is identity metadata only.
- **`404` (hide the resource) for a permission miss.** Rejected: the resource is
  in the caller's own org and already visible via the list endpoints, so `403`
  leaks nothing — the same reasoning ADR-0003 §1 uses. Cross-*org* references
  still `404`.
- **Fine-grained per-endpoint permissions** (e.g. `agents.create` vs
  `agents.publish`). Rejected for v1: the coarse `*.manage` verbs match how the
  console groups administration and keep the catalog small; a later ADR can split
  a verb if a real need appears, additively.

## Out of scope for this ADR (explicit)

- **Any OpenAPI or migration change.** The roles/assignment endpoints, the new
  `403` responses, the new tables, and the `members.role`/`entitlements`
  deprecation are **proposals** for later batches, not changes made here.
- **The concrete `RequirePermission` signature, caching strategy, and store
  interfaces** — owned by the implementing batch; this ADR fixes their
  properties (DB-sourced, per-request, fail-closed, union semantics), not their
  Go types.
- **Admin-console UX** for building and assigning roles.
- **Resource-scoped permissions beyond agents** (e.g. per-policy or per-team
  administration) — the model is agent-scoped for run access and org-scoped for
  management; finer scoping is a future ADR if needed.
- **Endpoint (`model_endpoints`) and budget write surfaces** — the
  `endpoints.manage`/`budgets.manage` permissions are reserved for them, but the
  endpoints themselves are defined elsewhere.
- **Signing-key / token-claim reshaping** beyond the decision that permissions
  are DB-sourced, not claim-sourced (an ADR-0001 follow-up).
- **Approval-routing policy** (who *must* approve what) — `approvals.decide`
  gates the decision endpoint; routing rules are a separate concern.

## References

- `migrations/000002_identity.up.sql` — `members`, `member_role` enum, `teams`,
  `team_memberships`.
- `migrations/000004_agent_catalog.up.sql` — `agents`, `agent_versions`,
  `policies`/`policy_versions`, `budgets`/`budget_rules`, `entitlements`.
- `internal/httpapi/launch.go` — §1 launch authorization
  (`resolveEntitlement`, `clampFanOut`), amended by §6 here.
- `internal/httpapi/agents.go`, `policies.go`, `approvals.go`, `analytics.go` —
  today's mutating handlers, authenticated but not authorized.
- `internal/authmw` — `RequireAuth`, `MustPrincipal`; `internal/auth` —
  `Claims{Subject, OrgID, MemberID, Roles}`, the JWT `roles` claim.
- `openapi/v1.yaml` — the operations mapped in §6; the roles/assignment
  additions and new `403`s proposed in §6–§7.
- [ADR-0001](0001-authentication-and-session-model.md) — Bearer JWT, principal
  in context, prompt revocation, fail-closed. (`roles`-claim follow-up, §5.)
- [ADR-0002](0002-rest-api-conventions.md) — org scoping, `403`-vs-`404` rule,
  naming contract↔behavior gaps as follow-ups.
- [ADR-0003](0003-session-launch-and-credential-grant.md) — session launch and
  the entitlement intersection this ADR amends (§1.3); the `policy_version_id`
  override (§1.4) and `clampFanOut` (§1) whose source moves to `role_agents`.
- `docs/security/threat-model.md` — least privilege, prompt revocation.
- `docs/ARCHITECTURE.md`, `docs/TECH_STACK.md` — planes and the locked stack.
