-- Stored notifications, per-person preferences and the send outbox (M6)
CREATE TABLE notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type       text NOT NULL,
    title      text NOT NULL,
    body       text NOT NULL DEFAULT '',
    data       jsonb NOT NULL DEFAULT '{}',
    priority   text NOT NULL DEFAULT 'normal' CHECK (priority IN ('silent','normal','critical')),
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_user_idx ON notifications (user_id, society_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id, society_id) WHERE read_at IS NULL;
CREATE INDEX notifications_created_idx ON notifications (created_at);

CREATE TABLE notification_prefs (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    grp     text NOT NULL CHECK (grp IN ('visits','system')),
    push    boolean NOT NULL DEFAULT true,
    email   boolean NOT NULL DEFAULT false,
    PRIMARY KEY (user_id, grp)
);

-- one row per device token, so a retry never repeats a send that already worked
CREATE TABLE notification_outbox (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel    text NOT NULL CHECK (channel IN ('push','sms','email')),
    target     text NOT NULL,
    payload    jsonb NOT NULL,
    attempts   smallint NOT NULL DEFAULT 0,
    next_at    timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_outbox_due_idx ON notification_outbox (next_at) WHERE sent_at IS NULL AND attempts < 5;
