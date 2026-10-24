INSERT INTO permissions (key, description) VALUES
  ('helper.manage','Register and manage my flat''s daily help'),
  ('helper.read','See my flat''s daily help'),
  ('helper.verify','Verify, suspend and blacklist daily help'),
  ('attendance.log','Log daily help and staff at the gate'),
  ('attendance.read','Read attendance sheets');
INSERT INTO role_permissions (role_key, permission_key) VALUES
  ('society_admin','helper.manage'),('society_admin','helper.read'),('society_admin','helper.verify'),('society_admin','attendance.log'),('society_admin','attendance.read'),
  ('committee','helper.read'),('committee','helper.verify'),('committee','attendance.read'),
  ('resident_owner','helper.manage'),('resident_owner','helper.read'),
  ('resident_tenant','helper.manage'),('resident_tenant','helper.read'),
  ('family_member','helper.read'),
  ('guard','attendance.log');

ALTER TABLE attachments DROP CONSTRAINT attachments_owner_type_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_owner_type_check CHECK (owner_type IN ('notice','complaint','complaint_event','amenity','helper','staff'));
