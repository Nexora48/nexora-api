CREATE TABLE invoices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id   uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id      uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    period       text NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    kind         text NOT NULL DEFAULT 'regular' CHECK (kind IN ('regular','adhoc','late_fee')),
    number       text,
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','issued','partially_paid','paid','void')),
    issued_at    timestamptz,
    due_date     date NOT NULL,
    total_paise  bigint NOT NULL DEFAULT 0 CHECK (total_paise >= 0),
    paid_paise   bigint NOT NULL DEFAULT 0 CHECK (paid_paise >= 0 AND paid_paise <= total_paise),
    note         text,
    idem_key     text,
    voided_by    uuid REFERENCES memberships(id),
    void_reason  text,
    created_by   uuid REFERENCES memberships(id),
    created_at   timestamptz NOT NULL DEFAULT now()
);
-- one regular invoice per flat per period (a voided one may be regenerated); one late fee per invoice per month
CREATE UNIQUE INDEX invoices_regular_uq ON invoices (unit_id, period) WHERE kind = 'regular' AND status <> 'void';
CREATE UNIQUE INDEX invoices_number_uq ON invoices (society_id, number) WHERE number IS NOT NULL;
CREATE UNIQUE INDEX invoices_idem_uq ON invoices (society_id, idem_key) WHERE idem_key IS NOT NULL;
CREATE INDEX invoices_unit_idx ON invoices (unit_id, due_date);
CREATE INDEX invoices_society_period_idx ON invoices (society_id, period, status);

CREATE TABLE invoice_lines (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id   uuid NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    head_id      uuid REFERENCES charge_heads(id) ON DELETE SET NULL,
    label        text NOT NULL,
    amount_paise bigint NOT NULL CHECK (amount_paise >= 0),
    source_type  text,
    source_id    uuid
);
CREATE INDEX invoice_lines_invoice_idx ON invoice_lines (invoice_id);

CREATE TABLE invoice_counters (
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    ym         text NOT NULL,
    last       int  NOT NULL DEFAULT 0,
    PRIMARY KEY (society_id, ym)
);

CREATE TABLE invoice_reminders (
    invoice_id uuid NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    sent_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (invoice_id, kind)
);

ALTER TABLE amenity_bookings ADD COLUMN invoice_id uuid REFERENCES invoices(id) ON DELETE SET NULL;
