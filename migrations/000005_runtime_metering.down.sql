BEGIN;

DROP TABLE IF EXISTS approval_requests;
DROP TABLE IF EXISTS usage_events;
DROP TABLE IF EXISTS credential_grants;
DROP TABLE IF EXISTS sessions;
DROP TYPE IF EXISTS approval_status;
DROP TYPE IF EXISTS session_status;

COMMIT;
