INSERT INTO permissions (key, description) VALUES
  ('guard.manage', 'Create and manage guards and gates'),
  ('gate.shift',   'Start and end a gate shift');
INSERT INTO role_permissions (role_key, permission_key) VALUES
  ('society_admin','guard.manage'),('committee','guard.manage'),('guard','gate.shift');
