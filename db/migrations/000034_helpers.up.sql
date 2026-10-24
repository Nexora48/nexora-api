-- daily help: one record per person per society, linked to the flats they work for
CREATE TABLE helpers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id    uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    full_name     text NOT NULL CHECK (length(full_name) BETWEEN 2 AND 80),
    phone         text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('maid','cook','driver','nanny','other')),
    photo_key     text,
    id_last4      text CHECK (id_last4 ~ '^[A-Za-z0-9]{4}$'),
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','verified','suspended','blacklisted')),
    status_reason text,
    pass_code     text NOT NULL,
    verified_by   uuid REFERENCES memberships(id),
    created_by    uuid REFERENCES memberships(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, phone),
    UNIQUE (society_id, pass_code)
);

CREATE TABLE helper_flats (
    helper_id    uuid NOT NULL REFERENCES helpers(id) ON DELETE CASCADE,
    unit_id      uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    added_by     uuid REFERENCES memberships(id),
    days         smallint[] NOT NULL DEFAULT '{}',
    window_from  time,
    window_to    time,
    notify_entry boolean NOT NULL DEFAULT true,
    notify_exit  boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (helper_id, unit_id)
);
CREATE INDEX helper_flats_unit_idx ON helper_flats (unit_id);

CREATE TABLE helper_ratings (
    helper_id  uuid NOT NULL REFERENCES helpers(id) ON DELETE CASCADE,
    unit_id    uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    rating     smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
    comment    text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (helper_id, unit_id)
);
