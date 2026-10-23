INSERT INTO permissions (key) VALUES ('unit.read'),('profile.manage'),('family.manage'),('vehicle.manage');
INSERT INTO role_permissions (role_key, permission_key) VALUES
  ('society_admin','unit.read'),('society_admin','profile.manage'),('society_admin','family.manage'),('society_admin','vehicle.manage'),
  ('committee','unit.read'),('committee','profile.manage'),
  ('resident_owner','unit.read'),('resident_owner','profile.manage'),('resident_owner','family.manage'),('resident_owner','vehicle.manage'),
  ('resident_tenant','unit.read'),('resident_tenant','profile.manage'),('resident_tenant','family.manage'),('resident_tenant','vehicle.manage'),
  ('family_member','unit.read'),('family_member','profile.manage'),
  ('guard','unit.read');
