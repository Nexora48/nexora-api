DROP TABLE IF EXISTS platform_broadcast_targets;
DROP TABLE IF EXISTS platform_broadcasts;
DROP INDEX IF EXISTS platform_users_invite_uq;
ALTER TABLE platform_users DROP COLUMN IF EXISTS disabled_at, DROP COLUMN IF EXISTS invite_expires_at, DROP COLUMN IF EXISTS invite_token_hash, DROP COLUMN IF EXISTS invited_by;
