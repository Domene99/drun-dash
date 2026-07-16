-- Reverse 000009_secrets: drop the encrypted secret vault table. The up
-- migration is additive (a new table plus its index) and does not touch
-- model_endpoints, so this returns the schema cleanly to its prior state.
BEGIN;

DROP TABLE IF EXISTS secrets;

COMMIT;
