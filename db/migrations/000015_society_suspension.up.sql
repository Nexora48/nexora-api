ALTER TABLE societies
    ADD COLUMN suspended_reason text,
    ADD COLUMN suspended_at     timestamptz,
    ADD COLUMN created_by       uuid REFERENCES platform_users(id) ON DELETE SET NULL;
