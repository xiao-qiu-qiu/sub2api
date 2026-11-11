-- Refuse ambiguous historical ownership before changing plans or their provenance.
-- Hold out concurrent binding writes until the new invariant is installed.
LOCK TABLE quality_rule_template_accounts IN SHARE ROW EXCLUSIVE MODE;
DO $$
BEGIN
 IF EXISTS (SELECT plan_id FROM quality_rule_template_accounts
            WHERE plan_id IS NOT NULL GROUP BY plan_id HAVING count(*) > 1) THEN
  RAISE EXCEPTION USING ERRCODE='23514',
   MESSAGE='quality template migration: shared non-NULL plan_id bindings require explicit resolution';
 END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS quality_rule_template_accounts_plan_unique
 ON quality_rule_template_accounts(plan_id) WHERE plan_id IS NOT NULL;

-- Immutable origin survives template deletion; current membership remains in the binding table.
-- No FK: deleting a template must not discard provenance or force independent plans
-- into the legacy manual-rule slot. Existing action snapshots are left untouched.
ALTER TABLE scheduled_test_plans ADD COLUMN IF NOT EXISTS source_template_id BIGINT;

UPDATE scheduled_test_plans p SET source_template_id=l.template_id
FROM quality_rule_template_accounts l
WHERE l.plan_id=p.id AND p.source_template_id IS NULL;

DROP INDEX IF EXISTS scheduled_test_quality_account_scope_unique;
CREATE UNIQUE INDEX scheduled_test_quality_account_scope_unique
 ON scheduled_test_plans (account_id,
   (CASE WHEN pelican_config->'quality'->>'action_on' = 'passed' THEN 'recovery'
      WHEN pelican_config->'quality'->>'action' = 'enable_bps' THEN 'bps'
      WHEN pelican_config->'quality'->>'action' = 'observe_only' THEN 'observation'
      ELSE 'quarantine' END))
 WHERE pelican_config->'quality' IS NOT NULL AND source_template_id IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS scheduled_test_template_account_unique
 ON scheduled_test_plans (source_template_id, account_id)
 WHERE source_template_id IS NOT NULL;

-- Stable per-account sync reasons; reads do not perform upstream model discovery.
ALTER TABLE quality_rule_templates ADD COLUMN IF NOT EXISTS sync_issues JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Keep pause authoritative even when model discovery or later propagation fails.
-- Claim/action already require enabled plans and a current plan version.
CREATE OR REPLACE FUNCTION quality_template_pause_plans() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 UPDATE scheduled_test_plans p
 SET enabled=false, updated_at=GREATEST(clock_timestamp(),p.updated_at+interval '1 microsecond')
 WHERE p.enabled AND EXISTS (SELECT 1 FROM quality_rule_template_accounts l
                            WHERE l.template_id=NEW.id AND l.plan_id=p.id);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS quality_template_pause_plans ON quality_rule_templates;
CREATE TRIGGER quality_template_pause_plans AFTER UPDATE OF enabled ON quality_rule_templates
 FOR EACH ROW WHEN (NOT NEW.enabled) EXECUTE FUNCTION quality_template_pause_plans();

-- Repair pre-existing paused-template drift without touching action snapshots.
UPDATE scheduled_test_plans p
 SET enabled=false,updated_at=GREATEST(clock_timestamp(),p.updated_at+interval '1 microsecond')
 WHERE p.enabled AND EXISTS (SELECT 1 FROM quality_rule_template_accounts l
 JOIN quality_rule_templates t ON t.id=l.template_id WHERE l.plan_id=p.id AND NOT t.enabled);
