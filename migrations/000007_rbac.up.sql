-- RBAC: org-scoped roles that bundle permissions and agent access, assigned to
-- members and teams. Implements ADR-0006. This migration is additive and
-- lossless: it seeds the four built-in roles per org, maps the coarse
-- members.role enum onto role assignments, and converts existing entitlements
-- into role -> agent grants. members.role and entitlements are RETAINED (not
-- dropped) for one release so the backfill is reversible; the API batch moves
-- authorization onto roles.
BEGIN;

-- ---------------------------------------------------------------------------
-- Schema (ADR-0006 §5)
-- ---------------------------------------------------------------------------

-- A role bundles permissions (role_permissions) and agent access (all_agents
-- flag or role_agents). Built-in roles have builtin = true and an immutable
-- name + permission set; custom roles are fully editable.
CREATE TABLE roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT,
    builtin     BOOLEAN NOT NULL DEFAULT FALSE,
    -- When true, the role grants access to every agent in the org (present and
    -- future) and the explicit role_agents set is ignored.
    all_agents  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX roles_org_id_idx ON roles (org_id);

-- A role's permission set. permission is free TEXT validated against the Go
-- catalog (ADR-0006 §2), not a PG enum, so the catalog can grow without an
-- ALTER TYPE migration. The composite PK dedupes automatically.
CREATE TABLE role_permissions (
    role_id    UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    permission TEXT NOT NULL,
    PRIMARY KEY (role_id, permission)
);

-- Indexed for the RequirePermission lookup ("does any of a member's roles grant
-- permission P").
CREATE INDEX role_permissions_permission_idx ON role_permissions (permission);

-- A role's explicit agent access, used when all_agents = false. Carries the two
-- optional per-grant tuning columns entitlements offered so the entitlements
-- backfill is lossless (ADR-0006 §3).
CREATE TABLE role_agents (
    role_id           UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    agent_id          UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    policy_version_id UUID REFERENCES policy_versions (id) ON DELETE SET NULL,
    max_fan_out       INTEGER,
    PRIMARY KEY (role_id, agent_id),
    CHECK (max_fan_out IS NULL OR max_fan_out >= 0)
);

-- Reverse lookup: "who may run this agent".
CREATE INDEX role_agents_agent_id_idx ON role_agents (agent_id);

-- A role assigned directly to a member.
CREATE TABLE member_roles (
    member_id UUID NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    role_id   UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    PRIMARY KEY (member_id, role_id)
);

-- Reverse lookup: "who holds this role".
CREATE INDEX member_roles_role_id_idx ON member_roles (role_id);

-- A role assigned to a team; every member of the team inherits it.
CREATE TABLE team_roles (
    team_id UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    role_id UUID NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, role_id)
);

-- Reverse lookup: "which teams hold this role".
CREATE INDEX team_roles_role_id_idx ON team_roles (role_id);

-- ---------------------------------------------------------------------------
-- Backfill step 1 — seed the four built-in roles per org (ADR-0006 §5.1).
-- ---------------------------------------------------------------------------
INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT o.id, 'Owner', 'Super-admin: every permission and access to all agents.', TRUE, TRUE
FROM organizations o;

INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT o.id, 'Admin', 'Manages the catalog, members, roles, approvals, and audit; does not run agents.', TRUE, FALSE
FROM organizations o;

INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT o.id, 'Auditor', 'Read-only access to the org-wide audit log, spend, and activity.', TRUE, FALSE
FROM organizations o;

INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT o.id, 'Member', 'May run the agents granted to them; no management permissions.', TRUE, FALSE
FROM organizations o;

-- Seed each built-in role's locked permission set (ADR-0006 §1). Owner gets
-- every permission; Admin all *.manage + approvals.decide + audit.view;
-- Auditor audit.view; Member agents.run.
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.permission
FROM roles r
CROSS JOIN (VALUES
    ('agents.manage'), ('policies.manage'), ('endpoints.manage'),
    ('budgets.manage'), ('members.manage'), ('roles.manage'),
    ('approvals.decide'), ('audit.view'), ('agents.run')
) AS p (permission)
WHERE r.builtin AND r.name = 'Owner';

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.permission
FROM roles r
CROSS JOIN (VALUES
    ('agents.manage'), ('policies.manage'), ('endpoints.manage'),
    ('budgets.manage'), ('members.manage'), ('roles.manage'),
    ('approvals.decide'), ('audit.view')
) AS p (permission)
WHERE r.builtin AND r.name = 'Admin';

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'audit.view'
FROM roles r
WHERE r.builtin AND r.name = 'Auditor';

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'agents.run'
FROM roles r
WHERE r.builtin AND r.name = 'Member';

-- ---------------------------------------------------------------------------
-- Backfill step 2 — map members.role onto built-in role assignments
-- (ADR-0006 §5.2). admin -> Admin, member -> Member, auditor -> Auditor.
-- ---------------------------------------------------------------------------
INSERT INTO member_roles (member_id, role_id)
SELECT m.id, r.id
FROM members m
JOIN roles r ON r.org_id = m.org_id AND r.builtin
WHERE (m.role = 'admin'   AND r.name = 'Admin')
   OR (m.role = 'member'  AND r.name = 'Member')
   OR (m.role = 'auditor' AND r.name = 'Auditor');

-- Ensure at least one Owner per org: assign Owner to the org's founding admin
-- (the earliest-created 'admin' member). Orgs with no admin get no Owner and
-- need a manual assignment; that gap is intentional and named in ADR-0006 §5.2.
INSERT INTO member_roles (member_id, role_id)
SELECT founding.id, r.id
FROM (
    SELECT DISTINCT ON (org_id) id, org_id
    FROM members
    WHERE role = 'admin'
    ORDER BY org_id, created_at, id
) AS founding
JOIN roles r ON r.org_id = founding.org_id AND r.builtin AND r.name = 'Owner';

-- ---------------------------------------------------------------------------
-- Backfill step 3 — convert entitlements into role -> agent access
-- (ADR-0006 §5.3). Each principal with entitlements gets one per-principal
-- custom role carrying agents.run plus a role_agents row per granted agent
-- (copying policy_version_id / max_fan_out). This preserves the member-XOR-team
-- split and every per-grant override losslessly; effective access is a union so
-- these compose with the built-in Member role rather than conflicting with it.
-- ---------------------------------------------------------------------------

-- 3a. Per-member custom roles. The member id embedded in the name guarantees
-- uniqueness within the org.
INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT DISTINCT m.org_id, 'Direct grants — ' || m.id::text,
       'Migrated per-member entitlement grants (ADR-0006 §5.3).', FALSE, FALSE
FROM entitlements e
JOIN members m ON m.id = e.member_id
WHERE e.member_id IS NOT NULL;

INSERT INTO role_permissions (role_id, permission)
SELECT DISTINCT r.id, 'agents.run'
FROM roles r
JOIN members m ON m.org_id = r.org_id AND r.name = 'Direct grants — ' || m.id::text
WHERE r.builtin = FALSE;

INSERT INTO role_agents (role_id, agent_id, policy_version_id, max_fan_out)
SELECT r.id, e.agent_id, e.policy_version_id, e.max_fan_out
FROM entitlements e
JOIN members m ON m.id = e.member_id
JOIN roles r ON r.org_id = m.org_id AND r.name = 'Direct grants — ' || m.id::text AND r.builtin = FALSE
WHERE e.member_id IS NOT NULL;

INSERT INTO member_roles (member_id, role_id)
SELECT m.id, r.id
FROM members m
JOIN roles r ON r.org_id = m.org_id AND r.name = 'Direct grants — ' || m.id::text AND r.builtin = FALSE;

-- 3b. Per-team custom roles (symmetric).
INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT DISTINCT t.org_id, 'Team grants — ' || t.id::text,
       'Migrated per-team entitlement grants (ADR-0006 §5.3).', FALSE, FALSE
FROM entitlements e
JOIN teams t ON t.id = e.team_id
WHERE e.team_id IS NOT NULL;

INSERT INTO role_permissions (role_id, permission)
SELECT DISTINCT r.id, 'agents.run'
FROM roles r
JOIN teams t ON t.org_id = r.org_id AND r.name = 'Team grants — ' || t.id::text
WHERE r.builtin = FALSE;

INSERT INTO role_agents (role_id, agent_id, policy_version_id, max_fan_out)
SELECT r.id, e.agent_id, e.policy_version_id, e.max_fan_out
FROM entitlements e
JOIN teams t ON t.id = e.team_id
JOIN roles r ON r.org_id = t.org_id AND r.name = 'Team grants — ' || t.id::text AND r.builtin = FALSE
WHERE e.team_id IS NOT NULL;

INSERT INTO team_roles (team_id, role_id)
SELECT t.id, r.id
FROM teams t
JOIN roles r ON r.org_id = t.org_id AND r.name = 'Team grants — ' || t.id::text AND r.builtin = FALSE;

COMMIT;
