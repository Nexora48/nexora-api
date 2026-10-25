ALTER TABLE platform_users
    ADD COLUMN invited_by         uuid REFERENCES platform_users(id),
    ADD COLUMN invite_token_hash  text,
    ADD COLUMN invite_expires_at  timestamptz,
    ADD COLUMN disabled_at        timestamptz;
CREATE UNIQUE INDEX platform_users_invite_uq ON platform_users (invite_token_hash) WHERE invite_token_hash IS NOT NULL;

CREATE TABLE platform_broadcasts (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title             text NOT NULL,
    body              text NOT NULL,
    link              text,
    audience          jsonb NOT NULL,
    include_residents boolean NOT NULL DEFAULT false,
    status            text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','sending','sent','cancelled')),
    recipients        int NOT NULL DEFAULT 0,
    sent_count        int NOT NULL DEFAULT 0,
    created_by        uuid REFERENCES platform_users(id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz
);
-- recipients are resolved once and drained in batches
CREATE TABLE platform_broadcast_targets (
    broadcast_id uuid NOT NULL REFERENCES platform_broadcasts(id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    society_id   uuid NOT NULL,
    sent_at      timestamptz,
    PRIMARY KEY (broadcast_id, user_id)
);
CREATE INDEX platform_broadcast_targets_due_idx ON platform_broadcast_targets (broadcast_id) WHERE sent_at IS NULL;
