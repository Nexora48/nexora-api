DELETE FROM role_permissions WHERE permission_key IN ('parking.manage','parking.read');
DELETE FROM permissions WHERE key IN ('parking.manage','parking.read');
DROP TABLE IF EXISTS parking_slots;
