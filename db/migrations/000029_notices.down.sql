DELETE FROM role_permissions WHERE role_key='guard' AND permission_key='notice.read';
DROP TABLE IF EXISTS notice_reads;
DROP TABLE IF EXISTS notice_blocks;
DROP TABLE IF EXISTS notices;
