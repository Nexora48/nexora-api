ALTER TABLE users
    ADD COLUMN date_of_birth date,
    ADD COLUMN gov_id_type   text,
    ADD COLUMN gov_id_last4  text;

CREATE TABLE emergency_contacts (
    user_id  uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    name     text NOT NULL,
    phone    text NOT NULL,
    relation text NOT NULL DEFAULT ''
);
