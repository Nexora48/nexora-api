-- plan catalogue: price, included services and limits (a missing limit key = unlimited)
CREATE TABLE plans (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key                 text NOT NULL UNIQUE CHECK (key ~ '^[a-z0-9_]{2,32}$'),
    name                text NOT NULL,
    rate_per_unit_paise bigint NOT NULL DEFAULT 0 CHECK (rate_per_unit_paise >= 0),
    flat_fee_paise      bigint NOT NULL DEFAULT 0 CHECK (flat_fee_paise >= 0),
    features            text[] NOT NULL DEFAULT '{}',
    limits              jsonb NOT NULL DEFAULT '{}',
    status              text NOT NULL DEFAULT 'active' CHECK (status IN ('active','archived')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

INSERT INTO plans (key, name, rate_per_unit_paise, flat_fee_paise, features, limits) VALUES
 ('legacy',   'Legacy (no limits)', 0,    0,      ARRAY['visitors','payments','complaints','community','amenities','daily_help','sos','deliveries','polls','staff_attendance'], '{}'),
 ('starter',  'Starter',            1000, 0,      ARRAY['visitors','complaints','community','sos','deliveries'],
    '{"max_units":100,"max_residents":400,"max_guards":5,"max_admins":3,"sms_per_month":500,"storage_mb":500,"visitors_per_month":3000}'),
 ('standard', 'Standard',           1500, 100000, ARRAY['visitors','payments','complaints','community','amenities','sos','deliveries','polls'],
    '{"max_units":500,"max_residents":2000,"max_guards":15,"max_admins":6,"sms_per_month":3000,"storage_mb":3000}'),
 ('premium',  'Premium',            2000, 500000, ARRAY['visitors','payments','complaints','community','amenities','daily_help','sos','deliveries','polls','staff_attendance'],
    '{"max_admins":20,"sms_per_month":20000,"storage_mb":20000}');
