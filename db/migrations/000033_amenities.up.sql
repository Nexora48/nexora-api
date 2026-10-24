CREATE TABLE amenities (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id            uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    name                  text NOT NULL CHECK (length(name) BETWEEN 2 AND 60),
    description           text NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    capacity              integer NOT NULL DEFAULT 1 CHECK (capacity BETWEEN 1 AND 500),
    slot_minutes          integer NOT NULL DEFAULT 60 CHECK (slot_minutes IN (30,60,90,120)),
    min_notice_minutes    integer NOT NULL DEFAULT 60 CHECK (min_notice_minutes >= 0),
    max_days_ahead        integer NOT NULL DEFAULT 14 CHECK (max_days_ahead BETWEEN 1 AND 90),
    max_upcoming_per_flat integer NOT NULL DEFAULT 2 CHECK (max_upcoming_per_flat BETWEEN 1 AND 20),
    max_per_week_per_flat integer NOT NULL DEFAULT 4 CHECK (max_per_week_per_flat BETWEEN 1 AND 50),
    cancel_cutoff_hours   integer NOT NULL DEFAULT 4 CHECK (cancel_cutoff_hours BETWEEN 0 AND 72),
    fee_amount            numeric(10,2) NOT NULL DEFAULT 0 CHECK (fee_amount >= 0),
    deposit_amount        numeric(10,2) NOT NULL DEFAULT 0 CHECK (deposit_amount >= 0),
    requires_approval     boolean NOT NULL DEFAULT false,
    active                boolean NOT NULL DEFAULT true,
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX amenities_name_uq ON amenities (society_id, lower(name));

-- a weekday without a row is a closed day
CREATE TABLE amenity_hours (
    amenity_id uuid NOT NULL REFERENCES amenities(id) ON DELETE CASCADE,
    weekday    smallint NOT NULL CHECK (weekday BETWEEN 0 AND 6), -- 0 = Sunday
    opens      time NOT NULL,
    closes     time NOT NULL,
    CHECK (opens < closes),
    PRIMARY KEY (amenity_id, weekday)
);

CREATE TABLE amenity_blackouts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    amenity_id uuid NOT NULL REFERENCES amenities(id) ON DELETE CASCADE,
    on_date    date NOT NULL,
    reason     text NOT NULL DEFAULT '' CHECK (length(reason) <= 120),
    UNIQUE (amenity_id, on_date)
);

CREATE TABLE amenity_bookings (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id              uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    amenity_id              uuid NOT NULL REFERENCES amenities(id),
    unit_id                 uuid NOT NULL REFERENCES units(id),
    booked_by_membership_id uuid NOT NULL REFERENCES memberships(id),
    slot_start              timestamptz NOT NULL,
    slot_end                timestamptz NOT NULL,
    status                  text NOT NULL CHECK (status IN ('pending','confirmed','rejected','cancelled','completed','no_show','expired')),
    people                  integer NOT NULL DEFAULT 1 CHECK (people BETWEEN 1 AND 100),
    note                    text,
    fee_amount              numeric(10,2) NOT NULL DEFAULT 0,
    deposit_amount          numeric(10,2) NOT NULL DEFAULT 0,
    charge_status           text NOT NULL DEFAULT 'none' CHECK (charge_status IN ('none','pending','billed','waived')),
    decided_by              uuid REFERENCES memberships(id),
    decision_reason         text,
    cancelled_by            text CHECK (cancelled_by IN ('flat','office')),
    cancel_reason           text,
    reminded                boolean NOT NULL DEFAULT false,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);
-- a flat cannot hold the same slot twice
CREATE UNIQUE INDEX amenity_bookings_flat_slot_uq ON amenity_bookings (amenity_id, slot_start, unit_id) WHERE status IN ('pending','confirmed');
CREATE INDEX amenity_bookings_slot_idx ON amenity_bookings (amenity_id, slot_start);
CREATE INDEX amenity_bookings_unit_idx ON amenity_bookings (unit_id, slot_start DESC);
CREATE INDEX amenity_bookings_society_idx ON amenity_bookings (society_id, slot_start DESC);
