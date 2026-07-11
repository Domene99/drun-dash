-- Core tenancy: organizations are the top-level tenant boundary; every other
-- row in the schema ultimately hangs off an organization. Environments scope
-- resources within an organization (e.g. prod vs. staging).
BEGIN;

CREATE TABLE organizations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE environments (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

CREATE INDEX environments_org_id_idx ON environments (org_id);

COMMIT;
