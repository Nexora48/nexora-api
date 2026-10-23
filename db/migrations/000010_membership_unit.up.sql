ALTER TABLE memberships DROP CONSTRAINT memberships_status_check;
ALTER TABLE memberships ADD CONSTRAINT memberships_status_check
    CHECK (status IN ('active','suspended','inactive'));
ALTER TABLE memberships ADD CONSTRAINT memberships_unit_fk
    FOREIGN KEY (unit_id) REFERENCES units(id) ON DELETE RESTRICT;
ALTER TABLE memberships
    ADD COLUMN move_in_date         date,
    ADD COLUMN agreement_end_date   date,
    ADD COLUMN ended_at             timestamptz,
    ADD COLUMN app_access           boolean NOT NULL DEFAULT true,
    ADD COLUMN can_invite_visitors  boolean NOT NULL DEFAULT true,
    ADD COLUMN can_pay_maintenance  boolean NOT NULL DEFAULT false,
    ADD COLUMN sos_enabled          boolean NOT NULL DEFAULT true;
-- one owner and one tenant per flat (suspended residents still hold the slot)
CREATE UNIQUE INDEX memberships_unit_role_active_uq
    ON memberships (unit_id, role_key)
    WHERE unit_id IS NOT NULL
      AND status IN ('active','suspended')
      AND role_key IN ('resident_owner','resident_tenant');
CREATE INDEX memberships_unit_idx ON memberships (society_id, unit_id, status);
