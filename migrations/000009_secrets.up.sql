-- Writable secret vault (ADR-0009): encrypted-at-rest provider credentials for
-- local/self-hosted deployments. A row holds an AES-256-GCM ciphertext and its
-- per-secret nonce; the master key lives only in the process environment
-- (DRUNDASH_VAULT_KEY), never in this table. A model_endpoints.secret_ref of
-- the form vault://db/<id> points at one of these rows (the DB vault backend).
-- Additive and reversible: model_endpoints already has secret_ref, so nothing
-- else changes.
BEGIN;

CREATE TABLE secrets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    -- AES-256-GCM ciphertext and the 12-byte random nonce it was sealed with.
    -- The raw provider key is never stored; only sealed bytes live here.
    ciphertext BYTEA NOT NULL,
    nonce      BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Secrets are always resolved and listed within an organization.
CREATE INDEX secrets_org_id_idx ON secrets (org_id);

COMMIT;
