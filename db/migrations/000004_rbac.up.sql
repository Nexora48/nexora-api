CREATE TABLE roles (
    key  text PRIMARY KEY,
    name text NOT NULL
);
CREATE TABLE permissions (
    key         text PRIMARY KEY,
    description text NOT NULL DEFAULT ''
);
CREATE TABLE role_permissions (
    role_key       text NOT NULL REFERENCES roles(key) ON DELETE CASCADE,
    permission_key text NOT NULL REFERENCES permissions(key) ON DELETE CASCADE,
    PRIMARY KEY (role_key, permission_key)
);

INSERT INTO roles (key, name) VALUES
  ('society_admin','Society Admin'),('committee','Committee Member'),
  ('resident_owner','Resident (Owner)'),('resident_tenant','Resident (Tenant)'),
  ('family_member','Family Member'),('guard','Security Guard');

INSERT INTO permissions (key) VALUES
  ('society.read'),('society.manage'),('resident.read'),('resident.manage'),
  ('visitor.read'),('visitor.create'),('visitor.approve'),('visitor.log'),
  ('notice.read'),('notice.manage'),
  ('complaint.create'),('complaint.read'),('complaint.manage'),
  ('bill.read'),('bill.manage'),('payment.pay'),
  ('amenity.book'),('amenity.manage'),
  ('gate.entry'),('sos.trigger'),('sos.respond'),
  ('staff.manage'),('report.read');

-- society_admin: everything
INSERT INTO role_permissions SELECT 'society_admin', key FROM permissions;

INSERT INTO role_permissions (role_key, permission_key) VALUES
  ('committee','society.read'),('committee','resident.read'),('committee','visitor.read'),
  ('committee','notice.read'),('committee','notice.manage'),
  ('committee','complaint.read'),('committee','complaint.manage'),
  ('committee','bill.read'),('committee','amenity.manage'),('committee','report.read'),
  ('committee','sos.respond'),

  ('resident_owner','society.read'),('resident_owner','visitor.read'),('resident_owner','visitor.create'),
  ('resident_owner','visitor.approve'),('resident_owner','notice.read'),
  ('resident_owner','complaint.create'),('resident_owner','complaint.read'),
  ('resident_owner','bill.read'),('resident_owner','payment.pay'),
  ('resident_owner','amenity.book'),('resident_owner','sos.trigger'),

  ('resident_tenant','society.read'),('resident_tenant','visitor.read'),('resident_tenant','visitor.create'),
  ('resident_tenant','visitor.approve'),('resident_tenant','notice.read'),
  ('resident_tenant','complaint.create'),('resident_tenant','complaint.read'),
  ('resident_tenant','bill.read'),('resident_tenant','payment.pay'),
  ('resident_tenant','amenity.book'),('resident_tenant','sos.trigger'),

  ('family_member','society.read'),('family_member','visitor.read'),('family_member','notice.read'),
  ('family_member','complaint.create'),('family_member','complaint.read'),
  ('family_member','amenity.book'),('family_member','sos.trigger'),

  ('guard','society.read'),('guard','visitor.read'),('guard','visitor.log'),
  ('guard','gate.entry'),('guard','sos.respond');
