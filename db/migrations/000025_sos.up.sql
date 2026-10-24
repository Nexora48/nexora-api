CREATE TABLE sos_events (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id              uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id                 uuid NOT NULL REFERENCES units(id),
    raised_by_membership_id uuid NOT NULL REFERENCES memberships(id),
    kind                    text NOT NULL CHECK (kind IN ('medical','fire','security','other')),
    note                    text,
    is_test                 boolean NOT NULL DEFAULT false,
    status                  text NOT NULL DEFAULT 'raised' CHECK (status IN ('raised','acknowledged','resolved','cancelled')),
    raised_at               timestamptz NOT NULL DEFAULT now(),
    last_alert_at           timestamptz NOT NULL DEFAULT now(),
    escalations             smallint NOT NULL DEFAULT 0,
    acknowledged_by         uuid REFERENCES memberships(id),
    acknowledged_at         timestamptz,
    resolved_by             uuid REFERENCES memberships(id),
    resolved_at             timestamptz,
    resolution_note         text,
    cancelled_at            timestamptz
);
CREATE INDEX sos_society_idx ON sos_events (society_id, raised_at DESC);
CREATE INDEX sos_open_idx ON sos_events (society_id, status) WHERE status IN ('raised','acknowledged');
CREATE INDEX sos_user_idx ON sos_events (raised_by_membership_id, raised_at DESC);

-- admins and committee respond to SOS too
INSERT INTO role_permissions (role_key, permission_key) VALUES ('society_admin','sos.respond') ON CONFLICT DO NOTHING;
