ALTER TABLE devices ADD COLUMN app text NOT NULL DEFAULT '';
CREATE INDEX devices_token_idx ON devices (fcm_token) WHERE fcm_token IS NOT NULL;
