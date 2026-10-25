-- owners may pay by default; the office enables it for a tenant
UPDATE memberships SET can_pay_maintenance = true WHERE role_key = 'resident_owner';

ALTER TABLE notification_prefs DROP CONSTRAINT notification_prefs_grp_check;
ALTER TABLE notification_prefs ADD CONSTRAINT notification_prefs_grp_check CHECK (grp IN ('visits','system','community','complaints','amenities','helpers','billing'));
