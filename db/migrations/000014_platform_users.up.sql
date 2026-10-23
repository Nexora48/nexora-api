CREATE TABLE platform_users (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email          text NOT NULL UNIQUE,
    full_name      text NOT NULL,
    password_hash  text NOT NULL,
    role           text NOT NULL CHECK (role IN ('super_admin','platform_support')),
    status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    totp_secret_enc text,
    totp_enabled   boolean NOT NULL DEFAULT false,
    last_login_at  timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE platform_recovery_codes (
    id        bigserial PRIMARY KEY,
    user_id   uuid NOT NULL REFERENCES platform_users(id) ON DELETE CASCADE,
    code_hash text NOT NULL,
    used_at   timestamptz
);
CREATE INDEX platform_recovery_codes_user_idx ON platform_recovery_codes (user_id);
CREATE TABLE platform_refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES platform_users(id) ON DELETE CASCADE,
    family_id   uuid NOT NULL,
    token_hash  text NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    replaced_by uuid,
    ip          text,
    user_agent  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX platform_refresh_family_idx ON platform_refresh_tokens (family_id);
