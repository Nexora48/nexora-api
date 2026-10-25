-- one row per society, day and metric; rebuilt idempotently by the nightly rollup
CREATE TABLE usage_daily (
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    day        date NOT NULL,
    metric     text NOT NULL,
    value      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (society_id, day, metric)
);
CREATE INDEX usage_daily_metric_idx ON usage_daily (metric, day);

-- who was active when, kept 30 days for the monthly distinct count
CREATE TABLE usage_active_users (
    society_id uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    day        date NOT NULL,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (society_id, day, user_id)
);
CREATE INDEX usage_active_users_day_idx ON usage_active_users (day);

CREATE INDEX IF NOT EXISTS refresh_tokens_created_idx ON refresh_tokens (created_at);
CREATE INDEX IF NOT EXISTS refresh_tokens_replaced_idx ON refresh_tokens (replaced_by) WHERE replaced_by IS NOT NULL;
