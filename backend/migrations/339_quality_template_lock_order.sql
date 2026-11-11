-- Keep all quality mutations in plan -> account order. Different templates
-- own disjoint plan rows, but can touch the same accounts in different orders.
-- Pre-lock both sets deterministically before the admission triggers run.
CREATE OR REPLACE FUNCTION quality_template_pause_plans() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE target_account_id BIGINT;
BEGIN
 PERFORM p.id FROM scheduled_test_plans p
 JOIN quality_rule_template_accounts l ON l.plan_id=p.id
 WHERE l.template_id=NEW.id ORDER BY p.id FOR UPDATE OF p;

 FOR target_account_id IN
  SELECT DISTINCT p.account_id FROM scheduled_test_plans p
  JOIN quality_rule_template_accounts l ON l.plan_id=p.id
  WHERE l.template_id=NEW.id ORDER BY p.account_id
 LOOP
  PERFORM id FROM accounts WHERE id=target_account_id FOR UPDATE;
 END LOOP;

 UPDATE scheduled_test_plans p
 SET enabled=false,updated_at=GREATEST(clock_timestamp(),p.updated_at+interval '1 microsecond')
 WHERE p.enabled AND EXISTS (SELECT 1 FROM quality_rule_template_accounts l
                            WHERE l.template_id=NEW.id AND l.plan_id=p.id);
 RETURN NEW;
END $$;
