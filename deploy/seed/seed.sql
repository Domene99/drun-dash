-- Local development seed data for the drun-dash control plane.
--
-- Populates a single demo organization (slug = 'demo') that exercises the full
-- schema shipped by migrations 000001-000004: tenancy, identity, model
-- infrastructure, and the agent catalog (policies, budgets, agents,
-- entitlements) plus the RBAC roles model shipped by migration 000007.
--
-- IDEMPOTENT: safe to run repeatedly. Every statement either targets a natural
-- unique key with ON CONFLICT, or guards itself with NOT EXISTS for the two
-- tables that have no unique constraint (routing_rules, budget_rules). Foreign
-- keys are resolved by natural-key lookup (never by hard-coded UUIDs), so the
-- script also repairs cleanly if a demo row already exists with a different id.
--
-- This is DEV seed data only. It is deliberately separate from migrations/ and
-- is never applied in production. No real secrets are stored: model endpoints
-- carry opaque `secret_ref` handles, and the policy version carries a
-- placeholder content_hash (not a signed bundle).
--
-- Apply with psql, e.g.:
--   psql "$DATABASE_URL" -f deploy/seed/seed.sql

\set ON_ERROR_STOP on

BEGIN;

-- ---------------------------------------------------------------------------
-- Tenancy: one demo organization with production + staging environments.
-- ---------------------------------------------------------------------------
INSERT INTO organizations (slug, name)
VALUES ('demo', 'Demo Organization')
ON CONFLICT (slug) DO UPDATE
    SET name = EXCLUDED.name;

INSERT INTO environments (org_id, name)
SELECT o.id, v.name
FROM organizations o
CROSS JOIN (VALUES ('production'), ('staging')) AS v (name)
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Identity: two teams, three members (one per role), and their memberships.
-- ---------------------------------------------------------------------------
INSERT INTO teams (org_id, name)
SELECT o.id, v.name
FROM organizations o
CROSS JOIN (VALUES ('Platform'), ('Research')) AS v (name)
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO NOTHING;

INSERT INTO members (org_id, idp_subject, email, display_name, role)
SELECT o.id, v.idp_subject, v.email, v.display_name, v.role::member_role
FROM organizations o
CROSS JOIN (VALUES
    ('demo|admin',   'admin@demo.example',   'Ada Admin',     'admin'),
    ('demo|member',  'member@demo.example',  'Marvin Member', 'member'),
    ('demo|auditor', 'auditor@demo.example', 'Aria Auditor',  'auditor')
) AS v (idp_subject, email, display_name, role)
WHERE o.slug = 'demo'
ON CONFLICT (org_id, idp_subject) DO UPDATE
    SET email        = EXCLUDED.email,
        display_name = EXCLUDED.display_name,
        role         = EXCLUDED.role,
        active       = TRUE;

-- Platform: admin + member. Research: member + auditor.
INSERT INTO team_memberships (team_id, member_id)
SELECT t.id, m.id
FROM organizations o
JOIN teams t   ON t.org_id = o.id
JOIN members m ON m.org_id = o.id
WHERE o.slug = 'demo'
  AND (
      (t.name = 'Platform' AND m.idp_subject IN ('demo|admin', 'demo|member'))
   OR (t.name = 'Research' AND m.idp_subject IN ('demo|member', 'demo|auditor'))
  )
ON CONFLICT (team_id, member_id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Model infrastructure: two endpoints and a routing rule steering Claude
-- traffic to the primary Anthropic endpoint. secret_ref values are opaque
-- handles into an external secret store, never real provider keys.
-- ---------------------------------------------------------------------------
INSERT INTO model_endpoints (org_id, name, base_url, provider, secret_ref)
SELECT o.id, v.name, v.base_url, v.provider, v.secret_ref
FROM organizations o
CROSS JOIN (VALUES
    ('anthropic-primary', 'https://api.anthropic.com', 'anthropic', 'vault://dev/anthropic'),
    ('openai-fallback',   'https://api.openai.com/v1', 'openai',    'vault://dev/openai')
) AS v (name, base_url, provider, secret_ref)
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO UPDATE
    SET base_url   = EXCLUDED.base_url,
        provider   = EXCLUDED.provider,
        secret_ref = EXCLUDED.secret_ref,
        active     = TRUE;

-- routing_rules has no natural unique key; guard against duplicate inserts.
INSERT INTO routing_rules (org_id, priority, match_json, endpoint_id)
SELECT o.id, 0, '{"model": "claude-*"}'::jsonb, e.id
FROM organizations o
JOIN model_endpoints e ON e.org_id = o.id AND e.name = 'anthropic-primary'
WHERE o.slug = 'demo'
  AND NOT EXISTS (
      SELECT 1 FROM routing_rules r
      WHERE r.org_id = o.id AND r.endpoint_id = e.id AND r.priority = 0
  );

-- ---------------------------------------------------------------------------
-- Policy: one policy with a single version whose content_json is a minimal
-- drun_config object (see docs/policy/DRUN_POLICY_BUNDLE.md). content_hash is a
-- placeholder; this seed does not produce a signed bundle.
-- ---------------------------------------------------------------------------
INSERT INTO policies (org_id, name, description, current_version)
SELECT o.id, 'standard-dev', 'Standard developer guardrails for the demo org.', 1
FROM organizations o
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO UPDATE
    SET description     = EXCLUDED.description,
        current_version = EXCLUDED.current_version;

INSERT INTO policy_versions (policy_id, version, content_json, content_hash, signing_json)
SELECT
    p.id,
    1,
    '{
        "domain_allowlist": ["github.com", "*.githubusercontent.com", "pypi.org", "registry.npmjs.org"],
        "bash_command_denylist": ["sudo"],
        "bash_timeout_ms": 300000,
        "fetch_timeout_ms": 60000,
        "max_sessions": 10
    }'::jsonb,
    'sha256:0000000000000000000000000000000000000000000000000000000000000000',
    NULL
FROM organizations o
JOIN policies p ON p.org_id = o.id AND p.name = 'standard-dev'
WHERE o.slug = 'demo'
ON CONFLICT (policy_id, version) DO UPDATE
    SET content_json = EXCLUDED.content_json,
        content_hash = EXCLUDED.content_hash;

-- ---------------------------------------------------------------------------
-- Budget: one budget with a single org-wide monthly rule (soft_cap <= hard_cap).
-- ---------------------------------------------------------------------------
INSERT INTO budgets (org_id, name, currency)
SELECT o.id, 'demo-monthly', 'USD'
FROM organizations o
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO NOTHING;

-- budget_rules has no natural unique key; guard against duplicate inserts.
INSERT INTO budget_rules (budget_id, scope, scope_id, window_period, soft_cap, hard_cap)
SELECT b.id, 'organization'::budget_scope, NULL, 'monthly'::budget_window, 800.00, 1000.00
FROM organizations o
JOIN budgets b ON b.org_id = o.id AND b.name = 'demo-monthly'
WHERE o.slug = 'demo'
  AND NOT EXISTS (
      SELECT 1 FROM budget_rules r
      WHERE r.budget_id = b.id
        AND r.scope = 'organization'::budget_scope
        AND r.window_period = 'monthly'::budget_window
        AND r.scope_id IS NULL
  );

-- ---------------------------------------------------------------------------
-- Agents: two agents, each with a version 1 that pins the standard-dev policy
-- version, the primary Anthropic endpoint, and the demo budget.
-- ---------------------------------------------------------------------------
INSERT INTO agents (org_id, name, description, current_version)
SELECT o.id, v.name, v.description, 1
FROM organizations o
CROSS JOIN (VALUES
    ('code-reviewer', 'Reviews pull requests against org guardrails.'),
    ('doc-writer',    'Drafts and maintains internal documentation.')
) AS v (name, description)
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO UPDATE
    SET description     = EXCLUDED.description,
        current_version = EXCLUDED.current_version;

INSERT INTO agent_versions (
    agent_id, version, system_prompt, allowed_tools,
    policy_version_id, endpoint_id, budget_id, fan_out_ceiling
)
SELECT
    a.id, 1, v.system_prompt, v.allowed_tools::jsonb,
    pv.id, e.id, b.id, v.fan_out_ceiling
FROM organizations o
JOIN agents a           ON a.org_id = o.id
JOIN policies p         ON p.org_id = o.id AND p.name = 'standard-dev'
JOIN policy_versions pv ON pv.policy_id = p.id AND pv.version = 1
JOIN model_endpoints e  ON e.org_id = o.id AND e.name = 'anthropic-primary'
JOIN budgets b          ON b.org_id = o.id AND b.name = 'demo-monthly'
JOIN (VALUES
    ('code-reviewer', 'You are a meticulous code reviewer. Flag correctness bugs and policy violations.', '["fs.read", "http.get"]', 3),
    ('doc-writer',    'You are a concise technical writer. Keep docs accurate and current.',              '["fs.read", "fs.write"]', 1)
) AS v (agent_name, system_prompt, allowed_tools, fan_out_ceiling)
    ON v.agent_name = a.name
WHERE o.slug = 'demo'
ON CONFLICT (agent_id, version) DO UPDATE
    SET system_prompt     = EXCLUDED.system_prompt,
        allowed_tools     = EXCLUDED.allowed_tools,
        policy_version_id = EXCLUDED.policy_version_id,
        endpoint_id       = EXCLUDED.endpoint_id,
        budget_id         = EXCLUDED.budget_id,
        fan_out_ceiling   = EXCLUDED.fan_out_ceiling;

-- ---------------------------------------------------------------------------
-- Entitlements: grant one agent to a team and one to a member (member XOR team).
-- These are RETAINED for one release (ADR-0006 §5) but are no longer the launch
-- authority; the role grants seeded below are. They are kept so the legacy read
-- endpoints (GET/PUT /agents/{id}/entitlements) still have data to show.
-- ---------------------------------------------------------------------------
-- code-reviewer -> Platform team.
INSERT INTO entitlements (agent_id, team_id, max_fan_out)
SELECT a.id, t.id, 2
FROM organizations o
JOIN agents a ON a.org_id = o.id AND a.name = 'code-reviewer'
JOIN teams t  ON t.org_id = o.id AND t.name = 'Platform'
WHERE o.slug = 'demo'
ON CONFLICT (agent_id, team_id) WHERE team_id IS NOT NULL DO NOTHING;

-- doc-writer -> Marvin Member.
INSERT INTO entitlements (agent_id, member_id, max_fan_out)
SELECT a.id, m.id, 1
FROM organizations o
JOIN agents a  ON a.org_id = o.id AND a.name = 'doc-writer'
JOIN members m ON m.org_id = o.id AND m.idp_subject = 'demo|member'
WHERE o.slug = 'demo'
ON CONFLICT (agent_id, member_id) WHERE member_id IS NOT NULL DO NOTHING;

-- ---------------------------------------------------------------------------
-- RBAC (ADR-0006): seed the four built-in roles for the demo org, map the
-- members onto them, and re-express the two entitlements above as role-based
-- agent access so launch authorization (now role-driven, ADR-0006 §6) keeps
-- working for the demo.
--
-- The migration (000007) seeds built-ins only for orgs that existed when it
-- ran; the demo org is created by THIS seed, which runs afterwards, so it must
-- seed its own built-in roles. Everything here is idempotent (ON CONFLICT on
-- the natural keys / composite PKs).
-- ---------------------------------------------------------------------------

-- Built-in roles (name + permission set are locked by ADR-0006 §1).
INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT o.id, v.name, v.description, TRUE, v.all_agents
FROM organizations o
CROSS JOIN (VALUES
    ('Owner',   'Super-admin: every permission and access to all agents.',                              TRUE),
    ('Admin',   'Manages the catalog, members, roles, approvals, and audit; does not run agents.',      FALSE),
    ('Auditor', 'Read-only access to the org-wide audit log, spend, and activity.',                     FALSE),
    ('Member',  'May run the agents granted to them; no management permissions.',                       FALSE)
) AS v (name, description, all_agents)
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO UPDATE
    SET description = EXCLUDED.description,
        builtin     = EXCLUDED.builtin,
        all_agents  = EXCLUDED.all_agents;

-- Built-in permission sets (ADR-0006 §1/§2).
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.permission
FROM organizations o
JOIN roles r ON r.org_id = o.id AND r.builtin AND r.name = 'Owner'
CROSS JOIN (VALUES
    ('agents.manage'), ('policies.manage'), ('endpoints.manage'),
    ('budgets.manage'), ('members.manage'), ('roles.manage'),
    ('approvals.decide'), ('audit.view'), ('agents.run')
) AS p (permission)
WHERE o.slug = 'demo'
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.permission
FROM organizations o
JOIN roles r ON r.org_id = o.id AND r.builtin AND r.name = 'Admin'
CROSS JOIN (VALUES
    ('agents.manage'), ('policies.manage'), ('endpoints.manage'),
    ('budgets.manage'), ('members.manage'), ('roles.manage'),
    ('approvals.decide'), ('audit.view')
) AS p (permission)
WHERE o.slug = 'demo'
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'audit.view'
FROM organizations o
JOIN roles r ON r.org_id = o.id AND r.builtin AND r.name = 'Auditor'
WHERE o.slug = 'demo'
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'agents.run'
FROM organizations o
JOIN roles r ON r.org_id = o.id AND r.builtin AND r.name = 'Member'
WHERE o.slug = 'demo'
ON CONFLICT (role_id, permission) DO NOTHING;

-- Map members onto built-in roles: admin -> Admin (+ Owner, the founding admin),
-- member -> Member, auditor -> Auditor (ADR-0006 §5.2).
INSERT INTO member_roles (member_id, role_id)
SELECT m.id, r.id
FROM organizations o
JOIN members m ON m.org_id = o.id
JOIN roles r   ON r.org_id = o.id AND r.builtin
WHERE o.slug = 'demo'
  AND (
      (m.idp_subject = 'demo|admin'   AND r.name = 'Admin')
   OR (m.idp_subject = 'demo|admin'   AND r.name = 'Owner')
   OR (m.idp_subject = 'demo|member'  AND r.name = 'Member')
   OR (m.idp_subject = 'demo|auditor' AND r.name = 'Auditor')
  )
ON CONFLICT (member_id, role_id) DO NOTHING;

-- Custom role "Code Reviewers": grants agents.run + access to the code-reviewer
-- agent (max_fan_out 2), assigned to the Platform team. Mirrors the migrated
-- code-reviewer -> Platform entitlement so the team can still launch it.
INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT o.id, 'Code Reviewers', 'Runs the code-reviewer agent (demo custom role).', FALSE, FALSE
FROM organizations o
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO UPDATE SET description = EXCLUDED.description;

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'agents.run'
FROM organizations o
JOIN roles r ON r.org_id = o.id AND r.name = 'Code Reviewers' AND r.builtin = FALSE
WHERE o.slug = 'demo'
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO role_agents (role_id, agent_id, policy_version_id, max_fan_out)
SELECT r.id, a.id, NULL, 2
FROM organizations o
JOIN roles r  ON r.org_id = o.id AND r.name = 'Code Reviewers' AND r.builtin = FALSE
JOIN agents a ON a.org_id = o.id AND a.name = 'code-reviewer'
WHERE o.slug = 'demo'
ON CONFLICT (role_id, agent_id) DO UPDATE SET max_fan_out = EXCLUDED.max_fan_out;

INSERT INTO team_roles (team_id, role_id)
SELECT t.id, r.id
FROM organizations o
JOIN teams t ON t.org_id = o.id AND t.name = 'Platform'
JOIN roles r ON r.org_id = o.id AND r.name = 'Code Reviewers' AND r.builtin = FALSE
WHERE o.slug = 'demo'
ON CONFLICT (team_id, role_id) DO NOTHING;

-- Custom role "Doc Writers": grants agents.run + access to the doc-writer agent
-- (max_fan_out 1), assigned directly to Marvin Member. Mirrors the migrated
-- doc-writer -> Marvin entitlement so the member can still launch it.
INSERT INTO roles (org_id, name, description, builtin, all_agents)
SELECT o.id, 'Doc Writers', 'Runs the doc-writer agent (demo custom role).', FALSE, FALSE
FROM organizations o
WHERE o.slug = 'demo'
ON CONFLICT (org_id, name) DO UPDATE SET description = EXCLUDED.description;

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'agents.run'
FROM organizations o
JOIN roles r ON r.org_id = o.id AND r.name = 'Doc Writers' AND r.builtin = FALSE
WHERE o.slug = 'demo'
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO role_agents (role_id, agent_id, policy_version_id, max_fan_out)
SELECT r.id, a.id, NULL, 1
FROM organizations o
JOIN roles r  ON r.org_id = o.id AND r.name = 'Doc Writers' AND r.builtin = FALSE
JOIN agents a ON a.org_id = o.id AND a.name = 'doc-writer'
WHERE o.slug = 'demo'
ON CONFLICT (role_id, agent_id) DO UPDATE SET max_fan_out = EXCLUDED.max_fan_out;

INSERT INTO member_roles (member_id, role_id)
SELECT m.id, r.id
FROM organizations o
JOIN members m ON m.org_id = o.id AND m.idp_subject = 'demo|member'
JOIN roles r   ON r.org_id = o.id AND r.name = 'Doc Writers' AND r.builtin = FALSE
WHERE o.slug = 'demo'
ON CONFLICT (member_id, role_id) DO NOTHING;

COMMIT;

\echo 'Seed complete: demo organization is ready.'
