-- Model infrastructure: registered upstream LLM endpoints and the priority-
-- ordered rules that route brokered traffic to them. Provider secrets are never
-- stored here; secret_ref points at an external secret store.
BEGIN;

CREATE TABLE model_endpoints (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    base_url   TEXT NOT NULL,
    provider   TEXT NOT NULL,
    -- Opaque handle into an external secret store (e.g. vault path / KMS key).
    -- The raw provider key is never persisted in this database.
    secret_ref TEXT NOT NULL,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX model_endpoints_org_id_idx ON model_endpoints (org_id);

CREATE TABLE routing_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- Lower priority values are evaluated first.
    priority    INTEGER NOT NULL DEFAULT 0,
    -- Match predicate (model name, labels, etc.) evaluated by the proxy.
    match_json  JSONB NOT NULL DEFAULT '{}'::jsonb,
    endpoint_id UUID NOT NULL REFERENCES model_endpoints (id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Rule evaluation walks an org's rules in priority order.
CREATE INDEX routing_rules_org_priority_idx ON routing_rules (org_id, priority);
CREATE INDEX routing_rules_endpoint_id_idx ON routing_rules (endpoint_id);

COMMIT;
