CREATE TABLE visits (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id              uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id                 uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    host_membership_id      uuid REFERENCES memberships(id) ON DELETE SET NULL,
    created_by_membership_id uuid REFERENCES memberships(id) ON DELETE SET NULL,
    visitor_name            text NOT NULL,
    visitor_phone           text,
    purpose                 text NOT NULL CHECK (purpose IN ('guest','delivery','cab','service')),
    company                 text,
    vehicle_number          text,
    visitor_count           int  NOT NULL DEFAULT 1 CHECK (visitor_count BETWEEN 1 AND 20),
    note                    text,
    source                  text NOT NULL CHECK (source IN ('pre_approved','walk_in')),
    status                  text NOT NULL CHECK (status IN ('invited','waiting','approved','inside','completed','rejected','cancelled','expired')),
    pass_code               text,
    expected_from           timestamptz NOT NULL,
    expected_until          timestamptz NOT NULL,
    entered_at              timestamptz,
    exited_at               timestamptz,
    decided_by_membership_id uuid REFERENCES memberships(id) ON DELETE SET NULL,
    decided_at              timestamptz,
    decision_reason         text,
    checked_in_by           uuid REFERENCES memberships(id) ON DELETE SET NULL,
    checked_out_by          uuid REFERENCES memberships(id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);
-- an OTP is unique among the society's *open* passes only
CREATE UNIQUE INDEX visits_open_pass_code_uq ON visits (society_id, pass_code) WHERE status = 'invited' AND pass_code IS NOT NULL;
CREATE INDEX visits_society_status_idx  ON visits (society_id, status, created_at DESC);
CREATE INDEX visits_society_unit_idx    ON visits (society_id, unit_id, created_at DESC);
CREATE INDEX visits_society_created_idx ON visits (society_id, created_at DESC);
