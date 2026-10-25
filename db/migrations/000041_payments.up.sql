CREATE TABLE payments (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id        uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id           uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    amount_paise      bigint NOT NULL CHECK (amount_paise > 0),
    unallocated_paise bigint NOT NULL DEFAULT 0 CHECK (unallocated_paise >= 0 AND unallocated_paise <= amount_paise),
    mode              text NOT NULL CHECK (mode IN ('razorpay','fake','cash','cheque','upi_transfer','bank_transfer','waiver')),
    reference         text,
    paid_on           date NOT NULL,
    received_by       uuid REFERENCES memberships(id),
    order_id          uuid,
    status            text NOT NULL DEFAULT 'ok' CHECK (status IN ('ok','reversed')),
    note              text,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_unit_idx ON payments (unit_id, created_at DESC);
CREATE INDEX payments_society_idx ON payments (society_id, paid_on DESC);
CREATE UNIQUE INDEX payments_order_uq ON payments (order_id) WHERE order_id IS NOT NULL;

CREATE TABLE payment_allocations (
    payment_id   uuid NOT NULL REFERENCES payments(id) ON DELETE CASCADE,
    invoice_id   uuid NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    amount_paise bigint NOT NULL CHECK (amount_paise > 0),
    PRIMARY KEY (payment_id, invoice_id)
);
CREATE INDEX payment_allocations_invoice_idx ON payment_allocations (invoice_id);

CREATE TABLE receipts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id uuid NOT NULL UNIQUE REFERENCES payments(id) ON DELETE CASCADE,
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    number     text NOT NULL,
    issued_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, number)
);
CREATE TABLE receipt_counters (
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    ym         text NOT NULL,
    last       int  NOT NULL DEFAULT 0,
    PRIMARY KEY (society_id, ym)
);

-- every movement of a flat's balance (positive = owes more, negative = owes less)
CREATE TABLE ledger_entries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id   uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id      uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('invoice','payment','waiver','reversal','void')),
    amount_paise bigint NOT NULL CHECK (amount_paise <> 0),
    invoice_id   uuid REFERENCES invoices(id) ON DELETE SET NULL,
    payment_id   uuid REFERENCES payments(id) ON DELETE SET NULL,
    reason       text,
    created_by   uuid REFERENCES memberships(id),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_unit_idx ON ledger_entries (unit_id, created_at);
