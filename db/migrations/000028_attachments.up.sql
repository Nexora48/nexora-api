-- one table for every uploaded file of notices and complaints (M7)
CREATE TABLE attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id   uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    owner_type   text NOT NULL CHECK (owner_type IN ('notice','complaint')),
    owner_id     uuid NOT NULL,
    blob_key     text NOT NULL,
    content_type text NOT NULL,
    size_bytes   integer NOT NULL CHECK (size_bytes > 0),
    filename     text NOT NULL,
    created_by   uuid REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_owner_idx ON attachments (owner_type, owner_id);
