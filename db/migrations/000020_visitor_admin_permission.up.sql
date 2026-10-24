-- society-wide visitor log (residents only see their own flat)
INSERT INTO permissions (key, description) VALUES ('visitor.admin', 'View the society-wide visitor log and export it');
INSERT INTO role_permissions (role_key, permission_key) VALUES ('society_admin','visitor.admin'),('committee','visitor.admin');
