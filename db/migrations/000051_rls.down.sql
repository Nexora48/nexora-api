DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT schemaname, tablename, policyname FROM pg_policies WHERE schemaname = 'public' AND policyname IN ('tenant_isolation', 'via_parent') LOOP
    EXECUTE format('DROP POLICY %I ON %I.%I', r.policyname, r.schemaname, r.tablename);
    EXECUTE format('ALTER TABLE %I.%I DISABLE ROW LEVEL SECURITY', r.schemaname, r.tablename);
  END LOOP;
END $$;
DROP FUNCTION IF EXISTS app_society();
-- platform-only tables had row security enabled without a policy
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['platform_invoices', 'platform_broadcast_targets', 'usage_daily', 'usage_active_users'] LOOP
    EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', t);
  END LOOP;
END $$;
