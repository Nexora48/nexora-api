CREATE TABLE memberships (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id    uuid, -- FK to units added in M2
    role_key   text NOT NULL REFERENCES roles(key),
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX memberships_user_society_unit_uq
    ON memberships (user_id, society_id, COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE INDEX memberships_society_idx ON memberships (society_id, role_key);
CREATE INDEX memberships_user_idx ON memberships (user_id);
