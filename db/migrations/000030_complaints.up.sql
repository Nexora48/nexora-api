CREATE TABLE complaint_counters (
    society_id  uuid PRIMARY KEY REFERENCES societies(id) ON DELETE CASCADE,
    last        integer NOT NULL DEFAULT 0,
    last_digest date
);

CREATE TABLE complaints (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id             uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    seq                    integer NOT NULL,
    unit_id                uuid NOT NULL REFERENCES units(id),
    raised_by_membership_id uuid NOT NULL REFERENCES memberships(id),
    title                  text NOT NULL CHECK (length(title) BETWEEN 1 AND 100),
    description            text NOT NULL CHECK (length(description) BETWEEN 1 AND 1000),
    category               text NOT NULL CHECK (category IN ('plumbing','electrical','lift','parking','security','cleaning','noise','other')),
    priority               text NOT NULL DEFAULT 'medium' CHECK (priority IN ('low','medium','high','urgent')),
    status                 text NOT NULL DEFAULT 'open' CHECK (status IN ('open','assigned','in_progress','resolved','closed')),
    visibility             text NOT NULL DEFAULT 'flat' CHECK (visibility IN ('flat','common')),
    assigned_membership_id uuid REFERENCES memberships(id),
    assigned_vendor        text,
    target_date            date,
    resolved_at            timestamptz,
    closed_at              timestamptz,
    rating                 smallint CHECK (rating BETWEEN 1 AND 5),
    rating_comment         text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, seq)
);
CREATE INDEX complaints_society_idx ON complaints (society_id, status, created_at DESC);
CREATE INDEX complaints_unit_idx ON complaints (unit_id, created_at DESC);
CREATE INDEX complaints_common_idx ON complaints (society_id, created_at DESC) WHERE visibility = 'common';

CREATE TABLE complaint_events (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    complaint_id        uuid NOT NULL REFERENCES complaints(id) ON DELETE CASCADE,
    society_id          uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    actor_membership_id uuid REFERENCES memberships(id),
    type                text NOT NULL,
    body                text NOT NULL DEFAULT '',
    internal            boolean NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX complaint_events_idx ON complaint_events (complaint_id, created_at);

ALTER TABLE attachments DROP CONSTRAINT attachments_owner_type_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_owner_type_check CHECK (owner_type IN ('notice','complaint','complaint_event'));
