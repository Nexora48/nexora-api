CREATE TABLE family_members (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id          uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id             uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    head_membership_id  uuid NOT NULL REFERENCES memberships(id) ON DELETE CASCADE,
    name                text NOT NULL,
    relation            text NOT NULL,
    phone               text,
    date_of_birth       date,
    login_membership_id uuid REFERENCES memberships(id) ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX family_members_unit_idx ON family_members (society_id, unit_id);
CREATE INDEX family_members_head_idx ON family_members (head_membership_id);

CREATE TABLE vehicles (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id    uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id       uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    owner_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    vehicle_type  text NOT NULL CHECK (vehicle_type IN ('car','bike','other')),
    number        text NOT NULL,
    make_model    text NOT NULL DEFAULT '',
    color         text NOT NULL DEFAULT '',
    sticker_no    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, number)
);
CREATE INDEX vehicles_unit_idx ON vehicles (society_id, unit_id);
