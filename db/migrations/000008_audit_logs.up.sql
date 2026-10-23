CREATE TABLE audit_logs (
    id         bigserial PRIMARY KEY,
    actor_id   uuid,
    actor_type text NOT NULL DEFAULT 'user',
    society_id uuid,
    action     text NOT NULL,
    entity     text NOT NULL DEFAULT '',
    entity_id  text NOT NULL DEFAULT '',
    meta       jsonb NOT NULL DEFAULT '{}',
    ip         text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_society_idx ON audit_logs (society_id, created_at DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_id, created_at DESC);
