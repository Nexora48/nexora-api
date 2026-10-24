CREATE TABLE polls (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id    uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    question      text NOT NULL CHECK (length(question) BETWEEN 3 AND 200),
    multi         boolean NOT NULL DEFAULT false,
    anonymous     boolean NOT NULL DEFAULT true,
    status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed','archived')),
    all_blocks    boolean NOT NULL DEFAULT true,
    closes_at     timestamptz,
    reminder_sent boolean NOT NULL DEFAULT false,
    created_by    uuid REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz
);
CREATE INDEX polls_society_idx ON polls (society_id, status, created_at DESC);

CREATE TABLE poll_options (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    poll_id  uuid NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
    label    text NOT NULL CHECK (length(label) BETWEEN 1 AND 80),
    position smallint NOT NULL
);
CREATE INDEX poll_options_idx ON poll_options (poll_id, position);

CREATE TABLE poll_blocks (
    poll_id  uuid NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
    block_id uuid NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
    PRIMARY KEY (poll_id, block_id)
);

-- one vote per flat (the flat's owner or tenant casts it)
CREATE TABLE poll_votes (
    poll_id   uuid NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
    unit_id   uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    option_id uuid NOT NULL REFERENCES poll_options(id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users(id),
    voted_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (poll_id, unit_id, option_id)
);

INSERT INTO permissions (key, description) VALUES
  ('poll.read','See polls'),('poll.vote','Vote in polls'),('poll.manage','Create and close polls');
INSERT INTO role_permissions (role_key, permission_key) VALUES
  ('society_admin','poll.read'),('society_admin','poll.vote'),('society_admin','poll.manage'),
  ('committee','poll.read'),('committee','poll.manage'),
  ('resident_owner','poll.read'),('resident_owner','poll.vote'),
  ('resident_tenant','poll.read'),('resident_tenant','poll.vote'),
  ('family_member','poll.read');
