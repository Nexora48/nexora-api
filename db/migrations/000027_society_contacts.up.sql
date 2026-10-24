-- emergency contacts the admin manages (replaces the hard-coded list in the resident app)
CREATE TABLE society_contacts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    label      text NOT NULL CHECK (length(label) BETWEEN 2 AND 60),
    phone      text NOT NULL CHECK (length(phone) BETWEEN 3 AND 15),
    hours      text NOT NULL DEFAULT '',
    sort_order integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX society_contacts_idx ON society_contacts (society_id, sort_order);
