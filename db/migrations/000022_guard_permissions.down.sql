DELETE FROM role_permissions WHERE permission_key IN ('guard.manage','gate.shift');
DELETE FROM permissions WHERE key IN ('guard.manage','gate.shift');
