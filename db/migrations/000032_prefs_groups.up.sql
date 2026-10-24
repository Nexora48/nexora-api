ALTER TABLE notification_prefs DROP CONSTRAINT notification_prefs_grp_check;
ALTER TABLE notification_prefs ADD CONSTRAINT notification_prefs_grp_check CHECK (grp IN ('visits','system','community','complaints'));
