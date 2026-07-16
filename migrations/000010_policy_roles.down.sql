-- Reverse 000010: drop the policy_roles join table. role_agents and
-- entitlements were retained by the up migration, so nothing else to restore.
BEGIN;

DROP TABLE IF EXISTS policy_roles;

COMMIT;
