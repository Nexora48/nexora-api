CREATE TABLE refresh_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    membership_id uuid NOT NULL REFERENCES memberships(id) ON DELETE CASCADE,
    family_id     uuid NOT NULL,
    token_hash    text NOT NULL UNIQUE,
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    replaced_by   uuid,
    device_id     text,
    ip            text,
    user_agent    text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

CREATE TABLE devices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id    text NOT NULL,
    platform     text NOT NULL DEFAULT '',
    fcm_token    text,
    app_version  text NOT NULL DEFAULT '',
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, device_id)
);
