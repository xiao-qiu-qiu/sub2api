-- Durable admission ownership is independent of disposable cooldown/usage fields.
-- Extra remains the only read needed by scheduling; old clients cannot replace it.
CREATE TABLE IF NOT EXISTS account_quality_probe_admissions (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    required_after TIMESTAMPTZ,
    checked_started_at TIMESTAMPTZ,
    checked_finished_at TIMESTAMPTZ,
    CHECK ((checked_started_at IS NULL AND checked_finished_at IS NULL) OR
           (checked_started_at IS NOT NULL AND checked_finished_at IS NOT NULL AND checked_finished_at >= checked_started_at))
);

CREATE OR REPLACE FUNCTION quality_probe_admission_snapshot_value(
    enabled BOOLEAN, required_after TIMESTAMPTZ, checked_started_at TIMESTAMPTZ, checked_finished_at TIMESTAMPTZ
) RETURNS JSONB LANGUAGE SQL STABLE AS $$
    SELECT jsonb_strip_nulls(jsonb_build_object('enabled', enabled,
        'required_after', to_char(required_after AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        'checked_started_at', to_char(checked_started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        'checked_finished_at', to_char(checked_finished_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')))
$$;

CREATE OR REPLACE FUNCTION quality_probe_admission_main_rule(config JSONB) RETURNS BOOLEAN
LANGUAGE SQL IMMUTABLE AS $$
    SELECT COALESCE(jsonb_typeof(config->'quality') = 'object'
        AND COALESCE(config->>'test_channel', '') <> 'bps'
        AND COALESCE(config->'quality'->>'action', '') NOT IN ('enable_bps', 'openai_two_fa_admission')
        AND COALESCE(config->'quality'->>'action_on', '') <> 'passed', FALSE)
$$;

CREATE OR REPLACE FUNCTION preserve_quality_probe_admission_snapshot() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE snapshot JSONB; base JSONB;
BEGIN
    base := CASE WHEN jsonb_typeof(NEW.extra) = 'object' THEN NEW.extra ELSE '{}'::jsonb END;
    SELECT quality_probe_admission_snapshot_value(enabled, required_after, checked_started_at, checked_finished_at)
      INTO snapshot FROM account_quality_probe_admissions WHERE account_id = NEW.id;
    -- Preserve scalar legacy Extra on unrelated writes until ownership exists.
    IF snapshot IS NOT NULL OR base ? 'quality_probe_admission' THEN
        NEW.extra := (base - 'quality_probe_admission') ||
            CASE WHEN snapshot IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('quality_probe_admission', snapshot) END;
    END IF;
    RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS preserve_quality_probe_admission_snapshot_trigger ON accounts;
CREATE TRIGGER preserve_quality_probe_admission_snapshot_trigger BEFORE INSERT OR UPDATE OF extra ON accounts
FOR EACH ROW EXECUTE FUNCTION preserve_quality_probe_admission_snapshot();

CREATE OR REPLACE FUNCTION quality_probe_admission_publish() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE snapshot JSONB;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NULL; END IF;
    snapshot := quality_probe_admission_snapshot_value(NEW.enabled, NEW.required_after, NEW.checked_started_at, NEW.checked_finished_at);
    -- Do not move updated_at: quality action ownership uses that account version.
    UPDATE accounts SET extra = (CASE WHEN jsonb_typeof(extra)='object' THEN extra ELSE '{}'::jsonb END) ||
        jsonb_build_object('quality_probe_admission', snapshot)
      WHERE id=NEW.account_id AND extra->'quality_probe_admission' IS DISTINCT FROM snapshot;
    IF FOUND THEN
        INSERT INTO scheduler_outbox(event_type, account_id) VALUES ('account_changed', NEW.account_id);
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS quality_probe_admission_publish_trigger ON account_quality_probe_admissions;
CREATE TRIGGER quality_probe_admission_publish_trigger AFTER INSERT OR UPDATE ON account_quality_probe_admissions
FOR EACH ROW EXECUTE FUNCTION quality_probe_admission_publish();

CREATE OR REPLACE FUNCTION quality_probe_admission_plan_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE aid BIGINT; active BOOLEAN;
BEGIN
    FOR aid IN SELECT DISTINCT id FROM unnest(ARRAY[
        CASE WHEN TG_OP <> 'INSERT' THEN OLD.account_id END,
        CASE WHEN TG_OP <> 'DELETE' THEN NEW.account_id END]) AS ids(id) WHERE id IS NOT NULL ORDER BY id
    LOOP
        PERFORM id FROM accounts WHERE id=aid FOR UPDATE;
        IF NOT FOUND THEN CONTINUE; END IF;
        SELECT EXISTS(SELECT 1 FROM scheduled_test_plans WHERE account_id=aid AND enabled AND NOT scope_suspended
            AND quality_probe_admission_main_rule(pelican_config)) INTO active;
        IF active OR EXISTS(SELECT 1 FROM account_quality_probe_admissions WHERE account_id=aid) THEN
            -- Rule administration changes participation, never recovery history.
            INSERT INTO account_quality_probe_admissions(account_id, enabled)
            VALUES (aid, active)
            ON CONFLICT(account_id) DO UPDATE SET enabled=EXCLUDED.enabled
            WHERE account_quality_probe_admissions.enabled IS DISTINCT FROM EXCLUDED.enabled;
        END IF;
    END LOOP;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS quality_probe_admission_plan_changed_trigger ON scheduled_test_plans;
CREATE TRIGGER quality_probe_admission_plan_changed_trigger
AFTER INSERT OR DELETE OR UPDATE OF enabled, scope_suspended, pelican_config, account_id ON scheduled_test_plans
FOR EACH ROW EXECUTE FUNCTION quality_probe_admission_plan_changed();

CREATE OR REPLACE FUNCTION quality_probe_admission_parse_time(value TEXT) RETURNS TIMESTAMPTZ LANGUAGE plpgsql STABLE AS $$
BEGIN
    IF value IS NULL OR value !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' THEN RETURN NULL; END IF;
    RETURN value::timestamptz;
EXCEPTION WHEN invalid_datetime_format OR datetime_field_overflow THEN RETURN NULL;
END $$;
CREATE OR REPLACE FUNCTION quality_probe_admission_number(value TEXT) RETURNS NUMERIC LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    value := btrim(value);
    IF value IS NULL OR value !~ '^-?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$' THEN RETURN NULL; END IF;
    RETURN value::numeric;
EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN RETURN NULL;
END $$;

-- Missing canonical durations retain legacy primary=5h/secondary=7d semantics.
-- Explicit invalid/different durations must never become account quota evidence.
CREATE OR REPLACE FUNCTION quality_probe_admission_codex_window(extra JSONB, p_window TEXT)
RETURNS TABLE(used NUMERIC, reset_at TIMESTAMPTZ) LANGUAGE plpgsql STABLE AS $$
DECLARE source TEXT := p_window; expected NUMERIC; observed TIMESTAMPTZ; seconds NUMERIC;
BEGIN
    expected := CASE WHEN p_window='5h' THEN 300 ELSE 10080 END;
    IF extra ? ('codex_' || p_window || '_window_minutes') AND
        quality_probe_admission_number(extra->>('codex_' || p_window || '_window_minutes')) IS DISTINCT FROM expected THEN RETURN; END IF;
    IF NOT extra ? ('codex_' || p_window || '_used_percent') THEN
        source := CASE WHEN p_window='5h' THEN 'primary' ELSE 'secondary' END;
        IF quality_probe_admission_number(extra->>'codex_primary_window_minutes') = expected THEN source := 'primary';
        ELSIF quality_probe_admission_number(extra->>'codex_secondary_window_minutes') = expected THEN source := 'secondary';
        ELSIF extra ? ('codex_' || source || '_window_minutes') THEN RETURN; END IF;
    END IF;
    used := quality_probe_admission_number(extra->>('codex_' || source || '_used_percent'));
    reset_at := quality_probe_admission_parse_time(extra->>('codex_' || source || '_reset_at'));
    IF reset_at IS NULL THEN
        observed := quality_probe_admission_parse_time(extra->>'codex_usage_updated_at');
        seconds := quality_probe_admission_number(extra->>('codex_' || source || '_reset_after_seconds'));
        IF observed IS NOT NULL AND seconds > 0 AND seconds <= 31536000 THEN reset_at := observed + make_interval(secs => seconds::double precision); END IF;
    END IF;
    RETURN NEXT;
END $$;

CREATE OR REPLACE FUNCTION quality_probe_admission_identity_value(doc JSONB, keys TEXT[]) RETURNS TEXT
LANGUAGE SQL IMMUTABLE AS $$
    SELECT btrim(doc->>key) FROM unnest(keys) WITH ORDINALITY AS fields(key, ord)
    WHERE jsonb_typeof(doc->key) IN ('string','number') AND btrim(doc->>key) <> '' ORDER BY ord LIMIT 1
$$;
CREATE OR REPLACE FUNCTION quality_probe_admission_codex_trusted(credentials JSONB, extra JSONB) RETURNS BOOLEAN
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE pair RECORD; left_value TEXT; right_value TEXT;
BEGIN
    FOR pair IN SELECT * FROM (VALUES
        (ARRAY['email'], ARRAY['email','email_address']),
        (ARRAY['chatgpt_account_id'], ARRAY['chatgpt_account_id','account_id']),
        (ARRAY['workspace_id','chatgpt_workspace_id','organization_id','org_id'], ARRAY['workspace_id','chatgpt_workspace_id','organization_id','org_id'])
    ) AS identities(left_keys, right_keys) LOOP
        left_value := quality_probe_admission_identity_value(credentials, pair.left_keys);
        right_value := quality_probe_admission_identity_value(extra, pair.right_keys);
        IF left_value IS NOT NULL AND right_value IS NOT NULL AND lower(left_value) <> lower(right_value) THEN RETURN FALSE; END IF;
    END LOOP;
    RETURN TRUE;
END $$;

CREATE OR REPLACE FUNCTION quality_probe_admission_setting(p_key TEXT) RETURNS JSONB LANGUAGE plpgsql STABLE AS $$
DECLARE config JSONB;
BEGIN
    SELECT value::jsonb INTO config FROM settings WHERE key=p_key;
    RETURN COALESCE(config, '{}'::jsonb);
EXCEPTION WHEN invalid_text_representation THEN RETURN '{}'::jsonb;
END $$;
CREATE OR REPLACE FUNCTION quality_probe_admission_bool(value JSONB) RETURNS BOOLEAN LANGUAGE SQL IMMUTABLE AS $$
    SELECT CASE jsonb_typeof(value)
        WHEN 'boolean' THEN value='true'::jsonb
        WHEN 'number' THEN COALESCE(quality_probe_admission_number(value #>> '{}') <> 0, FALSE)
        WHEN 'string' THEN btrim(value #>> '{}') IN ('1','t','T','TRUE','true','True')
        ELSE FALSE END
$$;

-- Reconstruct only thresholds which could actually stop this account. Merely
-- having used a quota window is not evidence of a recovery episode.
CREATE OR REPLACE FUNCTION quality_probe_admission_codex_paused(
    credentials JSONB, extra JSONB, p_window TEXT, used NUMERIC, shadow BOOLEAN, at_time TIMESTAMPTZ,
    global_ops JSONB DEFAULT NULL, global_thresholds JSONB DEFAULT NULL
) RETURNS BOOLEAN LANGUAGE plpgsql STABLE AS $$
DECLARE threshold NUMERIC; pause_threshold NUMERIC; reset_threshold NUMERIC; auto_reset BOOLEAN; credit JSONB; checked_at TIMESTAMPTZ;
BEGIN
    IF used IS NULL OR used <= 0 THEN RETURN FALSE; END IF;
    IF used >= 100 THEN RETURN TRUE; END IF;
    threshold := round(quality_probe_admission_number(credentials->>'account_scheduling_threshold'));
    IF threshold IS NULL OR threshold < 1 OR threshold > 100 THEN
        threshold := quality_probe_admission_number(COALESCE(global_thresholds, quality_probe_admission_setting('account_scheduling_thresholds'))->>'openai');
    END IF;
    IF threshold BETWEEN 1 AND 99 AND used >= threshold THEN RETURN TRUE; END IF;
    auto_reset := NOT shadow AND quality_probe_admission_bool(extra->'auto_reset_credit_enabled');
    IF auto_reset THEN
        reset_threshold := quality_probe_admission_number(extra->>('auto_reset_credit_'||p_window||'_threshold'));
        IF reset_threshold IS NULL OR NOT (reset_threshold=0 OR reset_threshold BETWEEN 0.001 AND 1) THEN reset_threshold := 1; END IF;
        IF reset_threshold > 0 AND used >= 100 * reset_threshold THEN RETURN TRUE; END IF;
    END IF;
    IF quality_probe_admission_bool(extra->('auto_pause_'||p_window||'_disabled')) THEN RETURN FALSE; END IF;
    pause_threshold := LEAST(1, GREATEST(0, COALESCE(quality_probe_admission_number(extra->>('auto_pause_'||p_window||'_threshold')),0)));
    IF pause_threshold=0 THEN
        pause_threshold := LEAST(1, GREATEST(0, COALESCE(quality_probe_admission_number(
            COALESCE(global_ops, quality_probe_admission_setting('ops_advanced_settings'))->'openai_account_quota_auto_pause'->>('default_threshold_'||p_window)),0)));
    END IF;
    IF pause_threshold<=0 OR used < 100 * pause_threshold THEN RETURN FALSE; END IF;
    -- Fresh available reset credits defer ordinary auto-pause until the
    -- consumption threshold; an ignored reset window still pauses normally.
    IF auto_reset AND reset_threshold > 0 THEN
        credit := extra->'codex_auto_reset_credit_state';
        checked_at := quality_probe_admission_parse_time(credit->>'checked_at');
        IF credit->>'status'='available' AND quality_probe_admission_number(credit->>'available_count')>0
            AND checked_at > at_time-interval '10 minutes' THEN RETURN FALSE; END IF;
    END IF;
    RETURN TRUE;
END $$;

CREATE OR REPLACE FUNCTION quality_probe_admission_local_reset(extra JSONB, dim TEXT) RETURNS TIMESTAMPTZ LANGUAGE plpgsql STABLE AS $$
DECLARE start_at TIMESTAMPTZ; reset_at TIMESTAMPTZ; tz TEXT; local_start TIMESTAMP; next_local TIMESTAMP; reset_hour INT; reset_day INT;
BEGIN
    start_at := quality_probe_admission_parse_time(extra->>('quota_'||dim||'_start'));
    IF start_at IS NULL THEN RETURN NULL; END IF;
    IF COALESCE(extra->>('quota_'||dim||'_reset_mode'),'rolling') <> 'fixed' THEN
        RETURN start_at + CASE WHEN dim='daily' THEN interval '1 day' ELSE interval '7 days' END;
    END IF;
    tz := COALESCE(NULLIF(extra->>'quota_reset_timezone',''), 'UTC');
    BEGIN local_start := start_at AT TIME ZONE tz;
    EXCEPTION WHEN invalid_parameter_value THEN tz := 'UTC'; local_start := start_at AT TIME ZONE tz; END;
    reset_hour := COALESCE(quality_probe_admission_number(extra->>('quota_'||dim||'_reset_hour')),0)::int;
    IF reset_hour NOT BETWEEN 0 AND 23 THEN RETURN NULL; END IF;
    next_local := date_trunc('day', local_start) + make_interval(hours=>reset_hour);
    IF dim='daily' THEN
        IF next_local<=local_start THEN next_local := next_local + interval '1 day'; END IF;
    ELSE
        reset_day := COALESCE(quality_probe_admission_number(extra->>'quota_weekly_reset_day'),1)::int;
        IF reset_day NOT BETWEEN 0 AND 6 THEN RETURN NULL; END IF;
        next_local := next_local + make_interval(days=>(reset_day - extract(dow FROM local_start)::int + 7)%7);
        IF next_local<=local_start THEN next_local := next_local + interval '7 days'; END IF;
    END IF;
    RETURN next_local AT TIME ZONE tz;
END $$;

CREATE OR REPLACE FUNCTION quality_probe_admission_account_recovered() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE boundary TIMESTAMPTZ; at_time TIMESTAMPTZ := clock_timestamp(); pair RECORD; p_window TEXT; old_window RECORD; new_window RECORD;
    old_observed TIMESTAMPTZ; new_observed TIMESTAMPTZ; recovered_at TIMESTAMPTZ; evaluated_at TIMESTAMPTZ; dim TEXT; old_limit NUMERIC; old_used NUMERIC; reset_at TIMESTAMPTZ;
BEGIN
    IF NOT EXISTS(SELECT 1 FROM account_quality_probe_admissions WHERE account_id=NEW.id) THEN RETURN NULL; END IF;
    IF (OLD.status <> 'active' AND NEW.status='active') OR (NOT OLD.schedulable AND NEW.schedulable)
       OR (OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL)
       OR (OLD.auto_pause_on_expired AND OLD.expires_at <= at_time AND
           (NOT NEW.auto_pause_on_expired OR NEW.expires_at IS NULL OR NEW.expires_at > at_time)) THEN
        boundary := at_time;
    END IF;
    FOR pair IN SELECT * FROM (VALUES
        (OLD.rate_limit_reset_at, NEW.rate_limit_reset_at),
        (OLD.temp_unschedulable_until, NEW.temp_unschedulable_until),
        (OLD.overload_until, NEW.overload_until)) AS deadlines(previous, current) LOOP
        IF pair.previous IS NOT NULL AND (pair.previous <= at_time OR pair.current IS NULL OR pair.current < pair.previous) THEN
            boundary := GREATEST(boundary, LEAST(pair.previous, at_time));
        END IF;
        IF pair.current <= at_time THEN boundary := GREATEST(boundary, pair.current); END IF;
    END LOOP;
    IF OLD.platform='openai' AND OLD.type='oauth' AND NEW.platform='openai' AND NEW.type='oauth'
       AND quality_probe_admission_codex_trusted(OLD.credentials, OLD.extra)
       AND quality_probe_admission_codex_trusted(NEW.credentials, NEW.extra) THEN
        old_observed := quality_probe_admission_parse_time(OLD.extra->>'codex_usage_updated_at');
        new_observed := quality_probe_admission_parse_time(NEW.extra->>'codex_usage_updated_at');
        FOREACH p_window IN ARRAY ARRAY['5h','7d'] LOOP
            SELECT * INTO old_window FROM quality_probe_admission_codex_window(OLD.extra, p_window);
            SELECT * INTO new_window FROM quality_probe_admission_codex_window(NEW.extra, p_window);
            reset_at := COALESCE(old_window.reset_at, old_observed + interval '2 hours');
            recovered_at := CASE WHEN new_observed > old_observed AND new_observed <= at_time THEN new_observed ELSE at_time END;
            evaluated_at := LEAST(reset_at, recovered_at) - interval '1 microsecond';
            IF quality_probe_admission_codex_paused(OLD.credentials, OLD.extra, p_window, old_window.used, OLD.parent_account_id IS NOT NULL, evaluated_at) THEN
                IF reset_at <= at_time THEN boundary := GREATEST(boundary, reset_at);
                ELSIF NOT quality_probe_admission_codex_paused(NEW.credentials, NEW.extra, p_window, new_window.used, NEW.parent_account_id IS NOT NULL, recovered_at) THEN
                    boundary := GREATEST(boundary, LEAST(reset_at, recovered_at));
                END IF;
            END IF;
        END LOOP;
    END IF;
    IF lower(OLD.type) IN ('apikey','bedrock','service_account') THEN
        FOREACH dim IN ARRAY ARRAY['daily','weekly'] LOOP
            old_limit := quality_probe_admission_number(OLD.extra->>('quota_'||dim||'_limit'));
            old_used := quality_probe_admission_number(OLD.extra->>('quota_'||dim||'_used'));
            IF old_limit > 0 AND old_used >= old_limit THEN
                reset_at := quality_probe_admission_local_reset(OLD.extra, dim);
                IF reset_at <= at_time THEN boundary := GREATEST(boundary, reset_at);
                ELSIF COALESCE(quality_probe_admission_number(NEW.extra->>('quota_'||dim||'_used')),0) < old_limit THEN
                    boundary := GREATEST(boundary, LEAST(reset_at, at_time));
                END IF;
            END IF;
        END LOOP;
        old_limit := quality_probe_admission_number(OLD.extra->>'quota_limit');
        IF old_limit > 0 AND quality_probe_admission_number(OLD.extra->>'quota_used') >= old_limit
           AND COALESCE(quality_probe_admission_number(NEW.extra->>'quota_used'),0) < old_limit THEN boundary := GREATEST(boundary, at_time); END IF;
    END IF;
    IF boundary IS NOT NULL THEN
        UPDATE account_quality_probe_admissions SET required_after=boundary
          WHERE account_id=NEW.id AND (required_after IS NULL OR required_after < boundary);
    END IF;
    RETURN NULL;
END $$;
-- Capture old global policy while it is still visible. No per-request query
-- is introduced: only explicit settings writes scan affected managed accounts.
CREATE OR REPLACE FUNCTION quality_probe_admission_settings_changed() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE account_row accounts%ROWTYPE; p_window TEXT; quota RECORD; at_time TIMESTAMPTZ := clock_timestamp();
    new_config JSONB := '{}'::jsonb; new_ops JSONB; new_thresholds JSONB; boundary TIMESTAMPTZ; reset_at TIMESTAMPTZ; evaluated_at TIMESTAMPTZ;
BEGIN
    IF OLD.key NOT IN ('ops_advanced_settings','account_scheduling_thresholds') THEN RETURN COALESCE(NEW,OLD); END IF;
    IF TG_OP='UPDATE' AND NEW.key=OLD.key AND NEW.value IS NOT DISTINCT FROM OLD.value THEN RETURN NEW; END IF;
    IF TG_OP='UPDATE' AND NEW.key=OLD.key THEN
        BEGIN new_config := COALESCE(NEW.value::jsonb, '{}'::jsonb);
        EXCEPTION WHEN invalid_text_representation THEN new_config := '{}'::jsonb; END;
    END IF;
    IF OLD.key='ops_advanced_settings' THEN new_ops:=new_config; ELSE new_thresholds:=new_config; END IF;
    FOR account_row IN SELECT a.* FROM accounts a JOIN account_quality_probe_admissions gate ON gate.account_id=a.id
        WHERE gate.enabled AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth'
        ORDER BY a.id FOR UPDATE OF a
    LOOP
        IF NOT quality_probe_admission_codex_trusted(account_row.credentials,account_row.extra) THEN CONTINUE; END IF;
        boundary := NULL;
        FOREACH p_window IN ARRAY ARRAY['5h','7d'] LOOP
            SELECT * INTO quota FROM quality_probe_admission_codex_window(account_row.extra,p_window);
            reset_at := COALESCE(quota.reset_at,quality_probe_admission_parse_time(account_row.extra->>'codex_usage_updated_at')+interval '2 hours');
            evaluated_at := LEAST(reset_at,at_time)-interval '1 microsecond';
            IF NOT quality_probe_admission_codex_paused(account_row.credentials,account_row.extra,p_window,quota.used,account_row.parent_account_id IS NOT NULL,evaluated_at) THEN CONTINUE; END IF;
            IF reset_at<=at_time THEN boundary:=GREATEST(boundary,reset_at);
            ELSIF NOT quality_probe_admission_codex_paused(account_row.credentials,account_row.extra,p_window,quota.used,account_row.parent_account_id IS NOT NULL,at_time,new_ops,new_thresholds) THEN
                boundary:=GREATEST(boundary,at_time);
            END IF;
        END LOOP;
        IF boundary IS NOT NULL THEN
            UPDATE account_quality_probe_admissions SET required_after=boundary
            WHERE account_id=account_row.id AND (required_after IS NULL OR required_after<boundary);
        END IF;
    END LOOP;
    RETURN COALESCE(NEW,OLD);
END $$;
DROP TRIGGER IF EXISTS quality_probe_admission_settings_changed_trigger ON settings;
CREATE TRIGGER quality_probe_admission_settings_changed_trigger BEFORE UPDATE OF key,value OR DELETE ON settings
FOR EACH ROW EXECUTE FUNCTION quality_probe_admission_settings_changed();

-- Do not close the entire existing pool on upgrade. Existing deadlines are
-- still visible to the service helper; rule enable/edit never creates a boundary.
-- Install the recovery trigger after backfill so publishing does not create
-- artificial recovery boundaries for existing accounts.
INSERT INTO account_quality_probe_admissions(account_id, enabled, required_after)
SELECT DISTINCT p.account_id, TRUE, NULL::timestamptz FROM scheduled_test_plans p
JOIN accounts a ON a.id=p.account_id AND a.deleted_at IS NULL
WHERE p.enabled AND NOT p.scope_suspended AND quality_probe_admission_main_rule(p.pelican_config)
ON CONFLICT(account_id) DO NOTHING;

DROP TRIGGER IF EXISTS quality_probe_admission_account_recovered_trigger ON accounts;
CREATE TRIGGER quality_probe_admission_account_recovered_trigger
AFTER UPDATE OF status, schedulable, rate_limit_reset_at, temp_unschedulable_until, overload_until,
    expires_at, auto_pause_on_expired, deleted_at, extra ON accounts
FOR EACH ROW EXECUTE FUNCTION quality_probe_admission_account_recovered();
