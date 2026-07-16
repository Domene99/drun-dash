-- Local authentication (ADR-0008): add an optional password to members, make
-- email unique per org (case-insensitive) so password login can resolve a
-- member by (org_id, lower(email)), and add the invitations table backing
-- member invites. Additive and reversible: OIDC members (password_hash NULL)
-- and the existing (org_id, idp_subject) NOT NULL unique index are untouched.
BEGIN;

-- A member may authenticate by OIDC (idp_subject) or email+password (this
-- hash) or both. NULL means "no password set" — any password login against it
-- fails with the generic auth error (ADR-0008 §1), never a special case.
ALTER TABLE members ADD COLUMN password_hash TEXT;

-- Email unique per org, case-insensitive (ADR-0008 §3a). Enables password login
-- by (org_id, lower(email)); OIDC members are unaffected (their key is the
-- subject, not the email). Precondition: no pre-existing per-org email
-- collisions (expected none, since OIDC keyed on subject, not email).
CREATE UNIQUE INDEX members_org_email_lower_idx ON members (org_id, lower(email));

-- Pending member invitations (ADR-0008 §6/§8). The invited member is created as
-- an inactive members row up front (members.active = false); this table carries
-- the single-use, TTL'd token — stored only as its SHA-256 hash, never the raw
-- value — and the roles to assign on acceptance.
CREATE TABLE invitations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    member_id   UUID NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL,                 -- SHA-256 of the token; never the raw value
    role_ids    UUID[] NOT NULL DEFAULT '{}',  -- pre-selected roles assigned on acceptance
    expires_at  TIMESTAMPTZ NOT NULL,          -- TTL (recommend 7 days)
    consumed_at TIMESTAMPTZ,                   -- non-null once accepted (single-use)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX invitations_token_hash_idx ON invitations (token_hash);
CREATE INDEX invitations_member_id_idx ON invitations (member_id);

COMMIT;
