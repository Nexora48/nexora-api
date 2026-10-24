DELETE FROM role_permissions WHERE permission_key IN ('poll.read','poll.vote','poll.manage');
DELETE FROM permissions WHERE key IN ('poll.read','poll.vote','poll.manage');
DROP TABLE IF EXISTS poll_votes;
DROP TABLE IF EXISTS poll_blocks;
DROP TABLE IF EXISTS poll_options;
DROP TABLE IF EXISTS polls;
