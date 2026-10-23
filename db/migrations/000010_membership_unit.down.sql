DROP INDEX IF EXISTS memberships_unit_idx;
DROP INDEX IF EXISTS memberships_unit_role_active_uq;
ALTER TABLE memberships
    DROP COLUMN sos_enabled, DROP COLUMN can_pay_maintenance, DROP COLUMN can_invite_visitors,
    DROP COLUMN app_access, DROP COLUMN ended_at, DROP COLUMN agreement_end_date, DROP COLUMN move_in_date;
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_unit_fk;
ALTER TABLE memberships DROP CONSTRAINT memberships_status_check;
ALTER TABLE memberships ADD CONSTRAINT memberships_status_check CHECK (status IN ('active','inactive'));
