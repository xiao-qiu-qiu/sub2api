-- A temporarily unmatched template member keeps its plan, history and ownership.
-- A NULL template binding remains a deliberate exclusion and is never resumed.
ALTER TABLE scheduled_test_plans ADD COLUMN IF NOT EXISTS scope_suspended BOOLEAN NOT NULL DEFAULT FALSE;

DROP INDEX IF EXISTS scheduled_test_quality_account_scope_unique;
CREATE UNIQUE INDEX scheduled_test_quality_account_scope_unique
 ON scheduled_test_plans (account_id,
   (CASE WHEN pelican_config->'quality'->>'action_on' = 'passed' THEN 'recovery'
      WHEN pelican_config->'quality'->>'action' = 'enable_bps' THEN 'bps'
      WHEN pelican_config->'quality'->>'action' = 'observe_only' THEN 'observation'
      ELSE 'quarantine' END))
 WHERE pelican_config->'quality' IS NOT NULL;
