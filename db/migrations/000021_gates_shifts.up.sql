-- Gates, guard shifts, walk-in photo/ring bookkeeping and idempotent gate events (M5)
CREATE TABLE gates (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 40),
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX gates_society_name_uq ON gates (society_id, lower(name));
INSERT INTO gates (society_id, name) SELECT id, 'Main Gate' FROM societies;

CREATE TABLE guard_shifts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id    uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    membership_id uuid NOT NULL REFERENCES memberships(id) ON DELETE CASCADE,
    gate_id       uuid NOT NULL REFERENCES gates(id),
    started_at    timestamptz NOT NULL DEFAULT now(),
    ended_at      timestamptz
);
CREATE UNIQUE INDEX guard_shifts_open_uq ON guard_shifts (membership_id) WHERE ended_at IS NULL;
CREATE INDEX guard_shifts_society_idx ON guard_shifts (society_id, started_at DESC);

ALTER TABLE visits
    ADD COLUMN gate_id         uuid REFERENCES gates(id),
    ADD COLUMN photo_key       text,
    ADD COLUMN awaiting_photo  boolean NOT NULL DEFAULT false,
    ADD COLUMN rerings         smallint NOT NULL DEFAULT 0,
    ADD COLUMN last_ring_at    timestamptz;

ALTER TABLE societies ADD COLUMN walkin_photo_required boolean NOT NULL DEFAULT true;

-- replay protection for actions queued offline on the guard device
CREATE TABLE gate_events (
    society_id      uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    client_event_id text NOT NULL CHECK (length(client_event_id) BETWEEN 8 AND 64),
    kind            text NOT NULL,
    http_status     integer NOT NULL,
    response        jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (society_id, client_event_id)
);
