CREATE TABLE features (
    key             text PRIMARY KEY,
    name            text NOT NULL,
    description     text NOT NULL DEFAULT '',
    default_enabled boolean NOT NULL DEFAULT true
);
CREATE TABLE society_features (
    society_id  uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    feature_key text NOT NULL REFERENCES features(key) ON DELETE CASCADE,
    enabled     boolean NOT NULL,
    changed_by  uuid,
    reason      text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (society_id, feature_key)
);
INSERT INTO features (key, name) VALUES
  ('visitors','Visitor Management'),('payments','Payments & Billing'),
  ('complaints','Complaints & Helpdesk'),('community','Notices & Community'),
  ('amenities','Amenity Booking'),('daily_help','Daily Help & Staff'),
  ('sos','Emergency / SOS'),('deliveries','Deliveries'),
  ('polls','Polls'),('staff_attendance','Staff Attendance');
