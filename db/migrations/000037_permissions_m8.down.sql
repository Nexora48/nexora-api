DELETE FROM attachments WHERE owner_type IN ('amenity','helper','staff');
ALTER TABLE attachments DROP CONSTRAINT attachments_owner_type_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_owner_type_check CHECK (owner_type IN ('notice','complaint','complaint_event'));
DELETE FROM role_permissions WHERE permission_key IN ('helper.manage','helper.read','helper.verify','attendance.log','attendance.read');
DELETE FROM permissions WHERE key IN ('helper.manage','helper.read','helper.verify','attendance.log','attendance.read');
