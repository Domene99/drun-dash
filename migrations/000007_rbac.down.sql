-- Reverse 000007_rbac: drop the RBAC tables, restoring members.role and
-- entitlements as the authorization source. Those tables were never modified by
-- the up migration (it is additive + backfill), so dropping the new tables
-- cleanly returns the schema to its prior state. Tables are dropped in reverse
-- dependency order (join tables before roles).
BEGIN;

DROP TABLE IF EXISTS team_roles;
DROP TABLE IF EXISTS member_roles;
DROP TABLE IF EXISTS role_agents;
DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS roles;

COMMIT;
