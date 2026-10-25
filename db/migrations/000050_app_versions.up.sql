CREATE TABLE app_versions (
    app            text PRIMARY KEY CHECK (app IN ('living','admin','get')),
    min_version    text NOT NULL DEFAULT '0.0.0',
    latest_version text NOT NULL DEFAULT '0.0.0',
    message        text NOT NULL DEFAULT '',
    store_url      text NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL DEFAULT now()
);
INSERT INTO app_versions (app) VALUES ('living'),('admin'),('get');
