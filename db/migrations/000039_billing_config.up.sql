-- what a society charges and how late payment is treated (all money in paise)
CREATE TABLE billing_settings (
    society_id      uuid PRIMARY KEY REFERENCES societies(id) ON DELETE CASCADE,
    due_day         int  NOT NULL DEFAULT 10 CHECK (due_day BETWEEN 1 AND 28),
    grace_days      int  NOT NULL DEFAULT 5 CHECK (grace_days BETWEEN 0 AND 60),
    late_fee_mode   text NOT NULL DEFAULT 'none' CHECK (late_fee_mode IN ('none','flat','percent')),
    late_fee_value  bigint NOT NULL DEFAULT 0 CHECK (late_fee_value >= 0), -- paise for flat, basis points (150 = 1.5%) for percent
    include_vacant  boolean NOT NULL DEFAULT false,
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE charge_heads (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id     uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    name           text NOT NULL CHECK (length(name) BETWEEN 2 AND 60),
    calc           text NOT NULL CHECK (calc IN ('fixed','per_unit_type','per_sqft')),
    amount_paise   bigint NOT NULL DEFAULT 0 CHECK (amount_paise >= 0),  -- fixed amount, or the default for per_unit_type
    rate_paise     bigint NOT NULL DEFAULT 0 CHECK (rate_paise >= 0),    -- per square foot
    recurring      boolean NOT NULL DEFAULT true,
    one_time_period text CHECK (one_time_period ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
    active         boolean NOT NULL DEFAULT true,
    sort_order     int NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (recurring OR one_time_period IS NOT NULL)
);
CREATE UNIQUE INDEX charge_heads_name_uq ON charge_heads (society_id, lower(name));

CREATE TABLE charge_head_rates (
    head_id      uuid NOT NULL REFERENCES charge_heads(id) ON DELETE CASCADE,
    unit_type    text NOT NULL,
    amount_paise bigint NOT NULL CHECK (amount_paise >= 0),
    PRIMARY KEY (head_id, unit_type)
);
