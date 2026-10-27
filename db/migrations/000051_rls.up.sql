-- Row-level security as a second lock behind the society_id filters in the services.
-- It only bites when a connection runs as the role nexora_app (DB_RLS=true: the API does SET ROLE nexora_app
-- and sets app.society_id from the signed-in user's token). Code without a signed-in user (sign-in, sweepers,
-- webhooks, the SuperAdmin console) runs with app.bypass = 'on'. With the role set and nothing else, no row is visible.

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'nexora_app') THEN
    CREATE ROLE nexora_app NOLOGIN;
  END IF;
END $$;

DO $$ BEGIN
  EXECUTE format('GRANT nexora_app TO %I', current_user);
END $$;

GRANT USAGE ON SCHEMA public TO nexora_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nexora_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO nexora_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO nexora_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO nexora_app;

CREATE FUNCTION app_society() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT nullif(current_setting('app.society_id', true), '')::uuid $$;
CREATE FUNCTION app_bypass() RETURNS boolean LANGUAGE sql STABLE AS
  $$ SELECT coalesce(current_setting('app.bypass', true), '') = 'on' $$;

-- 1. every table with a society_id: own rows only
DO $$
DECLARE t text;
BEGIN
  FOR t IN
    SELECT c.table_name FROM information_schema.columns c
    JOIN information_schema.tables tb ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name AND tb.table_type = 'BASE TABLE'
    WHERE c.table_schema = 'public' AND c.column_name = 'society_id'
      AND c.table_name NOT IN ('platform_invoices', 'platform_broadcast_targets', 'usage_daily', 'usage_active_users')
  LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (society_id = (SELECT app_society()) OR (SELECT app_bypass())) WITH CHECK (society_id = (SELECT app_society()) OR (SELECT app_bypass()))', t);
  END LOOP;
END $$;

-- 2. platform-only tables that happen to carry a society_id: tenants see nothing
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['platform_invoices', 'platform_broadcast_targets', 'usage_daily', 'usage_active_users'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY platform_only ON %I USING ((SELECT app_bypass())) WITH CHECK ((SELECT app_bypass()))', t);
  END LOOP;
END $$;

-- 3. the societies table itself: a tenant sees its own row
ALTER TABLE societies ENABLE ROW LEVEL SECURITY;
ALTER TABLE societies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON societies USING (id = (SELECT app_society()) OR (SELECT app_bypass())) WITH CHECK (id = (SELECT app_society()) OR (SELECT app_bypass()));

-- 4. child tables without a society_id follow their parent (the parent's own policy filters the lookup)
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT * FROM (VALUES
    ('amenity_blackouts', 'amenity_id', 'amenities'),
    ('amenity_hours', 'amenity_id', 'amenities'),
    ('charge_head_rates', 'head_id', 'charge_heads'),
    ('helper_flats', 'helper_id', 'helpers'),
    ('helper_ratings', 'helper_id', 'helpers'),
    ('invoice_lines', 'invoice_id', 'invoices'),
    ('invoice_reminders', 'invoice_id', 'invoices'),
    ('notice_blocks', 'notice_id', 'notices'),
    ('notice_reads', 'notice_id', 'notices'),
    ('payment_allocations', 'payment_id', 'payments'),
    ('poll_blocks', 'poll_id', 'polls'),
    ('poll_options', 'poll_id', 'polls'),
    ('poll_votes', 'poll_id', 'polls')
  ) AS v(child, fk, parent) LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', r.child);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', r.child);
    EXECUTE format('CREATE POLICY via_parent ON %I USING ((SELECT app_bypass()) OR EXISTS (SELECT 1 FROM %I p WHERE p.id = %I.%I)) WITH CHECK ((SELECT app_bypass()) OR EXISTS (SELECT 1 FROM %I p WHERE p.id = %I.%I))',
      r.child, r.parent, r.child, r.fk, r.parent, r.child, r.fk);
  END LOOP;
END $$;

-- Not covered on purpose (global or per-user, not per-society): users, devices, refresh_tokens, notification_prefs,
-- notification_outbox, emergency_contacts, roles, permissions, role_permissions, features, plans, app_versions,
-- audit_settings, platform_users, platform_recovery_codes, platform_refresh_tokens, platform_broadcasts.
