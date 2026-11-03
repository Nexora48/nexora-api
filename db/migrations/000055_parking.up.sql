-- Parking slots. The society office creates the slots (an area such as "Basement 1" plus a number) and allocates
-- each one to a flat; the residents of that flat see their slots in the resident app. Visitor slots are never allocated.
CREATE TABLE parking_slots (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id   uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    area         text NOT NULL CHECK (length(area) BETWEEN 1 AND 60),
    number       text NOT NULL CHECK (length(number) BETWEEN 1 AND 20),
    kind         text NOT NULL DEFAULT 'car' CHECK (kind IN ('car','bike','visitor')),
    note         text NOT NULL DEFAULT '' CHECK (length(note) <= 200),
    unit_id      uuid REFERENCES units(id) ON DELETE SET NULL,
    allocated_by uuid REFERENCES memberships(id) ON DELETE SET NULL,
    allocated_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (unit_id IS NULL OR kind <> 'visitor')
);
CREATE UNIQUE INDEX parking_slots_name_uq ON parking_slots (society_id, lower(area), lower(number));
CREATE INDEX parking_slots_unit_idx ON parking_slots (society_id, unit_id);

ALTER TABLE parking_slots ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON parking_slots USING (society_id = (SELECT app_society())) WITH CHECK (society_id = (SELECT app_society()));

-- Only the society admin manages slots; everyone with a flat can see their own.
INSERT INTO permissions (key, description) VALUES
  ('parking.manage', 'Create parking slots and allocate them to flats'),
  ('parking.read',   'See the parking slots allocated to my flat');
INSERT INTO role_permissions (role_key, permission_key) VALUES
  ('society_admin','parking.manage'),
  ('society_admin','parking.read'),
  ('committee','parking.read'),
  ('resident_owner','parking.read'),
  ('resident_tenant','parking.read'),
  ('family_member','parking.read');
