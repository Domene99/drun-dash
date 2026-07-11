-- Agent catalog: the governed, versioned definition of what agents may do.
-- Policies and budgets are versioned/parameterized building blocks; agents
-- compose them into runnable versions; entitlements grant principals access.
BEGIN;

-- ---------------------------------------------------------------------------
-- Policies: named governance boundaries with an append-only version history.
-- ---------------------------------------------------------------------------
CREATE TABLE policies (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    description     TEXT,
    -- Pointer to the newest version number; 0 until the first version lands.
    current_version INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX policies_org_id_idx ON policies (org_id);

CREATE TABLE policy_versions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id    UUID NOT NULL REFERENCES policies (id) ON DELETE CASCADE,
    version      INTEGER NOT NULL,
    -- Serialized policy boundary (allowed tools, network, fan-out, rules).
    content_json JSONB NOT NULL,
    -- Deterministic digest of content_json used for signing / integrity.
    content_hash TEXT NOT NULL,
    -- Detached signing metadata (algorithm, key_id, signature, signed_at).
    signing_json JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (policy_id, version),
    CHECK (version > 0)
);

-- ---------------------------------------------------------------------------
-- Budgets: spend envelopes with one or more scoped cap rules.
-- ---------------------------------------------------------------------------

-- Rolling window a budget rule resets over.
CREATE TYPE budget_window AS ENUM ('daily', 'weekly', 'monthly');
-- What a budget rule caps spend for.
CREATE TYPE budget_scope AS ENUM ('organization', 'team', 'agent', 'member');

CREATE TABLE budgets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    currency   TEXT NOT NULL DEFAULT 'USD',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX budgets_org_id_idx ON budgets (org_id);

CREATE TABLE budget_rules (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    budget_id     UUID NOT NULL REFERENCES budgets (id) ON DELETE CASCADE,
    scope         budget_scope NOT NULL,
    -- Optional target the scope applies to (e.g. a specific team/agent/member).
    -- NULL means the rule applies org-wide within the budget.
    scope_id      UUID,
    -- "window" is a reserved SQL keyword; stored as window_period.
    window_period budget_window NOT NULL,
    -- Soft cap warns; hard cap blocks. Amounts are in the budget's currency.
    soft_cap      NUMERIC(20, 6),
    hard_cap      NUMERIC(20, 6),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (soft_cap IS NULL OR soft_cap >= 0),
    CHECK (hard_cap IS NULL OR hard_cap >= 0),
    CHECK (soft_cap IS NULL OR hard_cap IS NULL OR soft_cap <= hard_cap)
);

CREATE INDEX budget_rules_budget_id_idx ON budget_rules (budget_id);

-- ---------------------------------------------------------------------------
-- Agents: named catalog entries with an append-only version history. Each
-- version pins a policy version, an endpoint, and (optionally) a budget.
-- ---------------------------------------------------------------------------
CREATE TABLE agents (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    description     TEXT,
    current_version INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX agents_org_id_idx ON agents (org_id);

CREATE TABLE agent_versions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id          UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    version           INTEGER NOT NULL,
    system_prompt     TEXT NOT NULL DEFAULT '',
    -- Tool allowlist for this version, e.g. ["fs.read", "http.get"].
    allowed_tools     JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- Governance building blocks are pinned by version; RESTRICT prevents
    -- deleting a policy version still referenced by a published agent version.
    policy_version_id UUID REFERENCES policy_versions (id) ON DELETE RESTRICT,
    endpoint_id       UUID REFERENCES model_endpoints (id) ON DELETE RESTRICT,
    budget_id         UUID REFERENCES budgets (id) ON DELETE SET NULL,
    fan_out_ceiling   INTEGER NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, version),
    CHECK (version > 0),
    CHECK (fan_out_ceiling >= 0)
);

CREATE INDEX agent_versions_policy_version_id_idx ON agent_versions (policy_version_id);
CREATE INDEX agent_versions_endpoint_id_idx ON agent_versions (endpoint_id);
CREATE INDEX agent_versions_budget_id_idx ON agent_versions (budget_id);

-- ---------------------------------------------------------------------------
-- Entitlements: grant an agent to exactly one principal (a member XOR a team).
-- ---------------------------------------------------------------------------
CREATE TABLE entitlements (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id          UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    member_id         UUID REFERENCES members (id) ON DELETE CASCADE,
    team_id           UUID REFERENCES teams (id) ON DELETE CASCADE,
    -- Optional policy-version override for this grant.
    policy_version_id UUID REFERENCES policy_versions (id) ON DELETE SET NULL,
    max_fan_out       INTEGER,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Exactly one principal: a member XOR a team.
    CONSTRAINT entitlements_member_xor_team CHECK (
        (member_id IS NOT NULL AND team_id IS NULL)
        OR (member_id IS NULL AND team_id IS NOT NULL)
    ),
    CHECK (max_fan_out IS NULL OR max_fan_out >= 0)
);

-- One grant per (agent, principal); partial indexes handle the nullable columns.
CREATE UNIQUE INDEX entitlements_agent_member_uniq
    ON entitlements (agent_id, member_id) WHERE member_id IS NOT NULL;
CREATE UNIQUE INDEX entitlements_agent_team_uniq
    ON entitlements (agent_id, team_id) WHERE team_id IS NOT NULL;
CREATE INDEX entitlements_member_id_idx ON entitlements (member_id) WHERE member_id IS NOT NULL;
CREATE INDEX entitlements_team_id_idx ON entitlements (team_id) WHERE team_id IS NOT NULL;

COMMIT;
