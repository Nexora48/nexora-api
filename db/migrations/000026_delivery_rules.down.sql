ALTER TABLE visits DROP COLUMN IF EXISTS handover, DROP COLUMN IF EXISTS decided_by_rule;
DROP TABLE IF EXISTS delivery_expectations;
DROP TABLE IF EXISTS delivery_rules;
