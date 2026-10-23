CREATE TABLE societies (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    slug       text NOT NULL UNIQUE,
    address    text NOT NULL DEFAULT '',
    city       text NOT NULL DEFAULT '',
    state      text NOT NULL DEFAULT '',
    pincode    text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active','trial','suspended')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
