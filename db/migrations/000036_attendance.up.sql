CREATE TABLE attendance_logs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id   uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    subject_type text NOT NULL CHECK (subject_type IN ('helper','staff')),
    subject_id   uuid NOT NULL,
    gate_id      uuid REFERENCES gates(id),
    in_at        timestamptz NOT NULL DEFAULT now(),
    out_at       timestamptz,
    in_by        uuid REFERENCES memberships(id),
    out_by       uuid REFERENCES memberships(id),
    source       text NOT NULL DEFAULT 'scan' CHECK (source IN ('scan','manual','auto_closed')),
    late         boolean NOT NULL DEFAULT false,
    off_hours    boolean NOT NULL DEFAULT false,
    note         text,
    CHECK (out_at IS NULL OR out_at >= in_at)
);
-- one open record per person: no second check-in while somebody is inside
CREATE UNIQUE INDEX attendance_open_uq ON attendance_logs (subject_type, subject_id) WHERE out_at IS NULL;
CREATE INDEX attendance_society_idx ON attendance_logs (society_id, in_at DESC);
CREATE INDEX attendance_subject_idx ON attendance_logs (subject_id, in_at DESC);
