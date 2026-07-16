# 0010 — Policy-centric access model

- **Status:** Accepted
- **Date:** 2026-07-16
- **Deciders:** drun-dash control-plane work stream; org owner
- **Batch:** 30
- **Supersedes:** the `role_agents` / `entitlements` agent-access portions of
  [ADR-0006](0006-rbac-and-access-model.md) (its permission model stands).

## Context

ADR-0006 established org-scoped roles that bundle a permission set and agent
access. Agent access, however, ended up split across two disconnected
mechanisms that never met:

- **`role_agents`** — per-role, per-agent grants (with optional
  `policy_version_id` / `max_fan_out` overrides). These are what the launch path
  (`AuthorizeRun`) consulted to decide whether a member could run an agent.
- **`entitlements`** — per-member/per-team, per-agent grants. These are what the
  member console's list (`GET /me/agents`) read to decide what a member could
  see.

In practice `entitlements` is empty: no surface writes it. So a member could be
authorized to launch an agent (via a `role_agents` grant) yet see an empty
member console — the bug the org owner hit. Two sources of truth for "which
agents can this member touch" is one too many, and neither matched how the owner
actually reasons about access.

The owner defined the intended model (confirmed): **an agent's access is decided
by its governing policy's permitted roles.** A policy already expresses the
governance boundary an agent runs under; letting that same policy name the roles
allowed to see and run agents governed by it collapses access to a single,
comprehensible rule.

## Decision

Access is resolved from **policy → roles**, in one predicate used by both the
launch check and the member console.

A member may **see and run** an agent iff they hold `agents.run` **and** one of:

1. the member holds an "all agents" super-admin role (`roles.all_agents`); or
2. the agent has **no governing policy** — general access; or
3. the agent's **governing policy permits one of the member's reachable roles**
   (a `policy_roles` row, where reachable roles are `member_roles` ∪ the
   member's teams' `team_roles`).

Otherwise access is denied (fail-closed).

Supporting definitions:

- An agent's **governing policy** is the `policy_id` of its current version's
  pinned `policy_version_id`
  (`agent_versions.policy_version_id` where `agent_id = ?` and
  `version = agents.current_version`, then `policy_versions.policy_id`). A null
  pin — or no current version — means **no policy → general access**.
- **No policy = general access**: an agent nobody has governed yet is visible and
  runnable by any member with `agents.run`. This keeps a freshly created,
  ungoverned agent usable without an extra grant step, and is the deliberate
  default the owner chose.
- **`roles.all_agents` is retained** unchanged: a super-admin sees and runs every
  agent in the org, present and future, regardless of policy.

The launch boundary (policy version, fan-out ceiling) now comes **only from the
agent version manifest**. The run grant no longer carries a `policy_version_id`
or `max_fan_out` override — those made sense only for per-role grants, which this
model retires. `AuthorizeRun` still gates the launch; the agent version is the
single authority for the boundary it runs under.

### Consequences

- `GET /me/agents` and `AuthorizeRun` share one predicate, so a member sees
  exactly the agents they can launch — the split that produced the empty-console
  bug is gone.
- A new join table `policy_roles(policy_id, role_id)` records a policy's
  permitted roles. A new `PUT /policies/{id}/roles` (permission `policies.manage`)
  sets them; `GET /policies/{id}/roles` reads them.
- **`role_agents` and `entitlements` are deprecated**: nothing consults them for
  access. They are **not dropped in this batch** so the admin UI and roles API
  keep compiling and running; a later UI batch removes their usage and a cleanup
  migration drops the tables.
- Migrating an org to the new model means, for each governed agent that should
  stay restricted, adding `policy_roles` rows for the intended roles. Agents left
  without policy roles are treated as general access — a widening, not a
  narrowing, so no member loses access silently on rollout; owners tighten access
  by attaching roles to the governing policy.

## Alternatives considered

- **Keep `role_agents`, backfill `entitlements` from it for the console.** Two
  tables stay authoritative and can drift again; it patches the symptom, not the
  split. Rejected.
- **Per-member/per-team entitlements as the single source.** This is the table
  that was already empty and unmanaged; it also scatters access decisions across
  principals rather than centralising them on the policy the owner already
  curates. Rejected.
- **Access on the agent row directly (agent → roles).** Bypasses the policy that
  already expresses governance, duplicating intent. Binding access to the policy
  keeps one object describing both what an agent may do and who may run it.
