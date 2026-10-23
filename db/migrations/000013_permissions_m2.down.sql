DELETE FROM role_permissions WHERE permission_key IN ('unit.read','profile.manage','family.manage','vehicle.manage');
DELETE FROM permissions WHERE key IN ('unit.read','profile.manage','family.manage','vehicle.manage');
