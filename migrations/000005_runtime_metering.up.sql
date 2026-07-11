-- Runtime and metering: launched sessions, the scoped proxy credentials minted
-- for them, append-only token usage, and the human approval/escalation queue.
BEGIN;

CREATE TYPE session_status AS ENUM ('pending', 'active', 'terminated', 'failed');
CREATE TYPE approval_status AS ENUM ('pending', 'approved', 'denied', 'expired');

-- ---------------------------------------------------------------------------
-- Sessions: a governed run of a specific agent version. agent_id uses RESTRICT
-- so historical sessions keep a valid reference to their agent.
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    agent_id          UUID NOT NULL REFERENCES agents (id) ON DELETE RESTRICT,
    agent_version     INTEGER,
    policy_version_id UUID REFERENCES policy_versions (id) ON DELETE SET NULL,
    environment_id    UUID REFERENCES environments (id) ON DELETE SET NULL,
    -- Member who launched the session, if attributable.
    launched_by       UUID REFERENCES members (id) ON DELETE SET NULL,
    status            session_status NOT NULL DEFAULT 'pending',
    fan_out_ceiling   INTEGER NOT NULL DEFAULT 1,
    labels            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    terminated_at     TIMESTAMPTZ,
    CHECK (fan_out_ceiling >= 0)
);

CREATE INDEX sessions_org_created_idx ON sessions (org_id, created_at);
CREATE INDEX sessions_agent_id_idx ON sessions (agent_id);
CREATE INDEX sessions_status_idx ON sessions (status);

-- ---------------------------------------------------------------------------
-- Credential grants: scoped, expiring proxy tokens. Only the hash of the token
-- is stored so a database leak does not expose usable credentials.
-- ---------------------------------------------------------------------------
CREATE TABLE credential_grants (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id       UUID NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    -- Hash (e.g. SHA-256) of the proxy_token; the raw token is never stored.
    proxy_token_hash TEXT NOT NULL,
    endpoint_id      UUID REFERENCES model_endpoints (id) ON DELETE SET NULL,
    scopes           JSONB NOT NULL DEFAULT '[]'::jsonb,
    issued_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,
    revoked_at       TIMESTAMPTZ
);

-- Token lookup: the proxy validates an incoming token by its hash on every call.
CREATE UNIQUE INDEX credential_grants_token_hash_uniq ON credential_grants (proxy_token_hash);
CREATE INDEX credential_grants_session_id_idx ON credential_grants (session_id);
CREATE INDEX credential_grants_expires_at_idx ON credential_grants (expires_at);

-- ---------------------------------------------------------------------------
-- Usage events: append-only token metering. High-volume, so keyed by a bigint
-- identity rather than a UUID. Never updated in place.
-- ---------------------------------------------------------------------------
CREATE TABLE usage_events (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id            UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    session_id        UUID REFERENCES sessions (id) ON DELETE SET NULL,
    agent_id          UUID REFERENCES agents (id) ON DELETE SET NULL,
    endpoint_id       UUID REFERENCES model_endpoints (id) ON DELETE SET NULL,
    model             TEXT,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER GENERATED ALWAYS AS (prompt_tokens + completion_tokens) STORED,
    cost_amount       NUMERIC(20, 6) NOT NULL DEFAULT 0,
    currency          TEXT NOT NULL DEFAULT 'USD',
    occurred_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (prompt_tokens >= 0),
    CHECK (completion_tokens >= 0)
);

-- Spend/analytics queries slice by org over a time range.
CREATE INDEX usage_events_org_occurred_idx ON usage_events (org_id, occurred_at);
CREATE INDEX usage_events_session_id_idx ON usage_events (session_id);

-- ---------------------------------------------------------------------------
-- Approval requests: the human escalation queue for gated actions.
-- ---------------------------------------------------------------------------
CREATE TABLE approval_requests (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    session_id   UUID NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    action       TEXT NOT NULL,
    reason       TEXT,
    context_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    status       approval_status NOT NULL DEFAULT 'pending',
    decided_by   UUID REFERENCES members (id) ON DELETE SET NULL,
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The queue view is "pending approvals for an org, oldest first".
CREATE INDEX approval_requests_org_status_created_idx
    ON approval_requests (org_id, status, created_at);
CREATE INDEX approval_requests_session_id_idx ON approval_requests (session_id);

COMMIT;
