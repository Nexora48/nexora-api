-- what Nexora charges each society (recorded by SuperAdmin, collected offline)
CREATE TABLE society_subscriptions (
    society_id          uuid PRIMARY KEY REFERENCES societies(id) ON DELETE CASCADE,
    rate_per_unit_paise bigint NOT NULL DEFAULT 0 CHECK (rate_per_unit_paise >= 0),
    flat_fee_paise      bigint NOT NULL DEFAULT 0 CHECK (flat_fee_paise >= 0),
    billing_day         int NOT NULL DEFAULT 1 CHECK (billing_day BETWEEN 1 AND 28),
    status              text NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused')),
    starts_on           date NOT NULL DEFAULT current_date,
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE platform_invoices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id   uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    period       text NOT NULL CHECK (period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    number       text NOT NULL,
    units        int NOT NULL,
    amount_paise bigint NOT NULL CHECK (amount_paise >= 0),
    status       text NOT NULL DEFAULT 'issued' CHECK (status IN ('issued','paid')),
    due_date     date NOT NULL,
    paid_on      date,
    reference    text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, period),
    UNIQUE (number)
);
