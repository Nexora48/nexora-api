CREATE TABLE notices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id   uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    title        text NOT NULL CHECK (length(title) BETWEEN 1 AND 120),
    body         text NOT NULL CHECK (length(body) BETWEEN 1 AND 4000),
    category     text NOT NULL CHECK (category IN ('general','maintenance','event','emergency','notice')),
    priority     text NOT NULL DEFAULT 'normal' CHECK (priority IN ('normal','important','urgent')),
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','scheduled','published','archived')),
    pinned       boolean NOT NULL DEFAULT false,
    all_blocks   boolean NOT NULL DEFAULT true,
    publish_at   timestamptz,
    published_at timestamptz,
    expires_at   timestamptz,
    edited_at    timestamptz,
    created_by   uuid REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notices_society_idx ON notices (society_id, status, published_at DESC);
CREATE INDEX notices_due_idx ON notices (publish_at) WHERE status = 'scheduled';

CREATE TABLE notice_blocks (
    notice_id uuid NOT NULL REFERENCES notices(id) ON DELETE CASCADE,
    block_id  uuid NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
    PRIMARY KEY (notice_id, block_id)
);

CREATE TABLE notice_reads (
    notice_id uuid NOT NULL REFERENCES notices(id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    unit_id   uuid REFERENCES units(id) ON DELETE SET NULL,
    read_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (notice_id, user_id)
);

-- guards read notices too
INSERT INTO role_permissions (role_key, permission_key) VALUES ('guard','notice.read') ON CONFLICT DO NOTHING;
