CREATE TABLE blocks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    name       text NOT NULL,
    code       text NOT NULL,
    sort_order int  NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, code)
);
CREATE TABLE units (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    block_id   uuid NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
    floor      int  NOT NULL,
    number     text NOT NULL,
    unit_type  text NOT NULL DEFAULT '2BHK',
    area_sqft  int,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, block_id, number)
);
CREATE INDEX units_block_floor_idx ON units (society_id, block_id, floor);
