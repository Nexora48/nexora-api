CREATE TABLE society_staff (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    full_name  text NOT NULL CHECK (length(full_name) BETWEEN 2 AND 80),
    phone      text NOT NULL,
    role       text NOT NULL CHECK (role IN ('housekeeping','gardener','electrician','plumber','supervisor','other')),
    photo_key  text,
    shift_from time NOT NULL DEFAULT '09:00',
    shift_to   time NOT NULL DEFAULT '17:00',
    work_days  smallint[] NOT NULL DEFAULT '{1,2,3,4,5,6}',
    pass_code  text NOT NULL,
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (society_id, pass_code),
    UNIQUE (society_id, phone)
);
