-- Telemetry: ingested trajectory metadata. A trajectory is the graph of a
-- governed run; trajectory_nodes are its nodes (the fork/rollback tree). By
-- default only *structure* is stored -- raw payloads are not persisted here.
BEGIN;

-- Mirrors pkg/trajectory FieldClass: structure-only vs. payload-bearing.
CREATE TYPE trajectory_field_class AS ENUM ('structure', 'payload');

CREATE TABLE trajectories (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    session_id    UUID REFERENCES sessions (id) ON DELETE CASCADE,
    -- Free-form trajectory-level metadata (labels, schema version, etc.).
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at      TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Telemetry queries slice by org over a time range.
CREATE INDEX trajectories_org_started_idx ON trajectories (org_id, started_at);
CREATE INDEX trajectories_session_id_idx ON trajectories (session_id);

CREATE TABLE trajectory_nodes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    trajectory_id UUID NOT NULL REFERENCES trajectories (id) ON DELETE CASCADE,
    -- Self-referential edge forming the fork/rollback tree; NULL at the root.
    parent_id     UUID REFERENCES trajectory_nodes (id) ON DELETE CASCADE,
    -- Client-supplied event id (unique within a trajectory).
    event_id      TEXT NOT NULL,
    -- Semantic kind, e.g. session_start / tool_call / policy_block / fork /
    -- rollback (kept as TEXT since the event-type set is intentionally open).
    event_type    TEXT NOT NULL,
    -- drun MCP tool name for tool_call / policy_block events, when applicable.
    tool_name     TEXT,
    -- Structure by default; payload only when raw content is explicitly stored.
    field_class   trajectory_field_class NOT NULL DEFAULT 'structure',
    -- Node-level structure/metadata. Raw payloads are NOT stored by default.
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at   TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trajectory_id, event_id)
);

CREATE INDEX trajectory_nodes_trajectory_id_idx ON trajectory_nodes (trajectory_id);
CREATE INDEX trajectory_nodes_parent_id_idx ON trajectory_nodes (parent_id);

COMMIT;
