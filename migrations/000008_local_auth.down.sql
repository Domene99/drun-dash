-- Reverse 000008_local_auth: drop the invitations table, the per-org
-- case-insensitive email index, and the members.password_hash column. The up
-- migration is additive, so this returns the schema cleanly to its prior state
-- — the (org_id, idp_subject) NOT NULL unique index and every OIDC member row
-- are untouched.
BEGIN;

DROP TABLE IF EXISTS invitations;
DROP INDEX IF EXISTS members_org_email_lower_idx;
ALTER TABLE members DROP COLUMN IF EXISTS password_hash;

COMMIT;
