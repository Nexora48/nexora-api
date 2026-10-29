-- the office dashboard counts entries per day
CREATE INDEX visits_society_entered_idx ON visits (society_id, entered_at DESC) WHERE entered_at IS NOT NULL;
