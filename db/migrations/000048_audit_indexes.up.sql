CREATE INDEX IF NOT EXISTS audit_logs_created_idx ON audit_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS audit_logs_action_idx ON audit_logs (action, created_at DESC);

CREATE TABLE audit_settings (
    id             boolean PRIMARY KEY DEFAULT true CHECK (id),
    retention_days int NOT NULL DEFAULT 400 CHECK (retention_days >= 30)
);
INSERT INTO audit_settings DEFAULT VALUES;
