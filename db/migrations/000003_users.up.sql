CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    full_name     text NOT NULL,
    phone         text NOT NULL UNIQUE,
    email         text UNIQUE,
    password_hash text,
    avatar_url    text,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    last_login_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
