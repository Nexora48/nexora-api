CREATE TABLE delivery_rules (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id    uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    scope      text NOT NULL CHECK (scope IN ('delivery','cab')),
    mode       text NOT NULL CHECK (mode IN ('ask','auto_approve','leave_at_gate')),
    companies  text[] NOT NULL DEFAULT '{}',
    updated_by uuid REFERENCES memberships(id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (unit_id, scope)
);

-- "I am expecting a delivery": a one-off auto-approve for one company
CREATE TABLE delivery_expectations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id    uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    company    text NOT NULL,
    created_by uuid REFERENCES memberships(id),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX delivery_expectations_open_idx ON delivery_expectations (unit_id) WHERE used_at IS NULL;

ALTER TABLE visits ADD COLUMN decided_by_rule boolean NOT NULL DEFAULT false, ADD COLUMN handover text;
