-- Self-registration: someone installs the resident app, verifies their phone, picks a society (by its join code)
-- and a flat, and the office approves or rejects. Admin-added residents never go through this.
ALTER TABLE societies
    ADD COLUMN join_code    text NOT NULL DEFAULT upper(substr(md5(gen_random_uuid()::text), 1, 8)),
    ADD COLUMN join_enabled boolean NOT NULL DEFAULT true;
CREATE UNIQUE INDEX societies_join_code_uq ON societies (join_code);

CREATE TABLE join_requests (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id     uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id        uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    phone          text NOT NULL,
    full_name      text NOT NULL CHECK (length(full_name) BETWEEN 2 AND 100),
    requested_type text NOT NULL CHECK (requested_type IN ('owner','tenant')),
    status         text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','cancelled')),
    reason         text,
    decided_by     uuid REFERENCES memberships(id) ON DELETE SET NULL,
    decided_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
-- one open request per person per society
CREATE UNIQUE INDEX join_requests_pending_uq ON join_requests (society_id, phone) WHERE status = 'pending';
CREATE INDEX join_requests_society_idx ON join_requests (society_id, status, created_at DESC);
CREATE INDEX join_requests_phone_idx ON join_requests (phone, created_at DESC);

ALTER TABLE join_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON join_requests USING (society_id = (SELECT app_society())) WITH CHECK (society_id = (SELECT app_society()));
