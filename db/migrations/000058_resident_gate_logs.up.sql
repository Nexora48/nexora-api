-- When a guard scans a resident's gate pass the entry or exit is logged here. Directions alternate: in, out, in...
CREATE TABLE resident_gate_logs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id    uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    membership_id uuid NOT NULL REFERENCES memberships(id) ON DELETE CASCADE,
    unit_id       uuid REFERENCES units(id) ON DELETE SET NULL,
    direction     text NOT NULL CHECK (direction IN ('in','out')),
    recorded_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX resident_gate_logs_member_idx ON resident_gate_logs (society_id, membership_id, created_at DESC);

ALTER TABLE resident_gate_logs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON resident_gate_logs USING (society_id = (SELECT app_society())) WITH CHECK (society_id = (SELECT app_society()));
