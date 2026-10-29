DROP TABLE IF EXISTS join_requests;
DROP INDEX IF EXISTS societies_join_code_uq;
ALTER TABLE societies DROP COLUMN IF EXISTS join_enabled, DROP COLUMN IF EXISTS join_code;
