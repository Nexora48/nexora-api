DELETE FROM attachments WHERE owner_type='complaint_event';
ALTER TABLE attachments DROP CONSTRAINT attachments_owner_type_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_owner_type_check CHECK (owner_type IN ('notice','complaint'));
DROP TABLE IF EXISTS complaint_events;
DROP TABLE IF EXISTS complaints;
DROP TABLE IF EXISTS complaint_counters;
