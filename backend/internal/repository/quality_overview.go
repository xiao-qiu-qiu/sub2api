package repository

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// One statement/snapshot for count, page, membership, plans and round evidence.
// Each plan's indexed newest-result scan is capped at 60*100: a valid quality
// round has at most 100 samples. A cutoff partial round can only be unknown.
// LATERAL joins are internal to PostgreSQL, not per-account network queries.
const qualityOverviewSQL = `
WITH eligible_plans AS NOT MATERIALIZED (
 SELECT p.* FROM scheduled_test_plans p
 JOIN accounts a ON a.id=p.account_id AND a.deleted_at IS NULL
 WHERE $7::text='all' OR (
   jsonb_typeof(p.pelican_config->'quality')='object'
   AND COALESCE(p.pelican_config->'quality'->>'action','')<>'openai_two_fa_admission'
   AND (COALESCE(p.pelican_config->'quality'->>'action_on','')='passed')=($7='recovery')
   AND (CASE WHEN p.pelican_config->>'question_kind'='state_probe' THEN 'account'
     WHEN p.pelican_config->>'test_channel'='bps' THEN 'bps' ELSE 'account' END)=$6
   -- A retained template binding is not membership evidence. Recheck the same
   -- current-account predicates as qualityTemplateAccountsQuery, including
   -- derived statuses, even before the background membership sync runs.
   AND NOT EXISTS (
     SELECT 1 FROM quality_rule_template_accounts l
     JOIN quality_rule_templates t ON t.id=l.template_id
     WHERE l.plan_id=p.id AND NOT (
       l.account_id=a.id
       AND (COALESCE(t.account_filter->>'type','')='' OR a.type=t.account_filter->>'type')
       AND (COALESCE(t.account_filter->>'search','')=''
         OR strpos(lower(a.name),lower(t.account_filter->>'search'))>0)
       AND (COALESCE(t.account_filter->>'group','')=''
         OR (t.account_filter->>'group'='ungrouped'
           AND NOT EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id))
         OR EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id AND ag.group_id::text=t.account_filter->>'group'))
       AND (COALESCE(t.account_filter->'statuses','[]'::jsonb)='[]'::jsonb
         OR EXISTS (
           SELECT 1 FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(t.account_filter->'statuses')='array'
             THEN t.account_filter->'statuses' ELSE '[]'::jsonb END) selected(status)
           WHERE CASE selected.status
             WHEN 'active' THEN a.status='active' AND a.schedulable
               AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at<=NOW())
               AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=NOW())
             WHEN 'rate_limited' THEN a.status='active' AND a.rate_limit_reset_at>NOW()
               AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=NOW())
             WHEN 'temp_unschedulable' THEN a.status='active' AND a.temp_unschedulable_until>NOW()
             WHEN 'unschedulable' THEN a.status='active' AND NOT a.schedulable
               AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at<=NOW())
               AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=NOW())
             ELSE a.status=selected.status END
         ))
     )
   )
 )
), filtered AS MATERIALIZED (
 SELECT a.id,a.name,a.platform,a.type FROM accounts a
 WHERE a.deleted_at IS NULL
 AND ($1='' OR a.name ILIKE $2 ESCAPE '\' OR a.id::text=$1)
 AND ($3::bigint IS NULL
   OR ($3=0 AND NOT EXISTS (SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL WHERE ag.account_id=a.id))
   OR EXISTS (SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL WHERE ag.account_id=a.id AND ag.group_id=$3))
 AND ($7='all' OR EXISTS (SELECT 1 FROM eligible_plans p WHERE p.account_id=a.id))
), page AS (
 SELECT * FROM filtered ORDER BY id DESC LIMIT $4 OFFSET $5
), account_items AS (
 SELECT a.id, jsonb_build_object('account_id',a.id,'name',a.name,'platform',a.platform,'type',a.type,
 'groups',COALESCE(members.items,'[]'::jsonb),'plans',COALESCE(plans.items,'[]'::jsonb),
 'rounds',COALESCE(history.items,'[]'::jsonb)) AS item
 FROM page a
 LEFT JOIN LATERAL (
   SELECT jsonb_agg(jsonb_build_object('id',g.id,'name',g.name) ORDER BY g.id) AS items
   FROM account_groups ag JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL WHERE ag.account_id=a.id
 ) members ON true
 LEFT JOIN LATERAL (
   SELECT jsonb_agg(jsonb_build_object('id',p.id,'enabled',p.enabled,'running_until',p.running_until,
   'next_run_at',p.next_run_at,'action',COALESCE(p.pelican_config->'quality'->>'action',''),
   'action_on',CASE WHEN p.pelican_config->'quality'->>'action_on'='passed' THEN 'passed' ELSE 'failed' END,
   'model_id',p.model_id,'model_ids',CASE WHEN jsonb_typeof(p.pelican_config->'model_ids')='array' AND p.pelican_config->'model_ids'<>'[]'::jsonb
     THEN p.pelican_config->'model_ids' ELSE jsonb_build_array(p.model_id) END,
   'test_channel',CASE WHEN p.pelican_config->>'question_kind'='state_probe' THEN 'account'
     WHEN p.pelican_config->>'test_channel'='bps' THEN 'bps' ELSE 'account' END,
   'auto_restore',COALESCE(p.pelican_config->'quality'->>'auto_restore'='true',false),
   'recovery_probe_enabled',COALESCE(p.pelican_config->'quality'->>'recovery_probe_enabled'='true',false),
   'recovery_probe_interval_minutes',CASE WHEN COALESCE(p.pelican_config->'quality'->>'recovery_probe_interval_minutes','') ~ '^[0-9]{1,5}$'
     THEN COALESCE(NULLIF((p.pelican_config->'quality'->>'recovery_probe_interval_minutes')::int,0),60) ELSE 60 END,
   'recovery_confirmation_minutes',CASE WHEN COALESCE(p.pelican_config->'quality'->>'recovery_confirmation_minutes','') ~ '^[0-9]{1,5}$'
     THEN COALESCE(NULLIF((p.pelican_config->'quality'->>'recovery_confirmation_minutes')::int,0),5) ELSE 5 END) ORDER BY p.id) AS items
   FROM eligible_plans p WHERE p.account_id=a.id AND jsonb_typeof(p.pelican_config->'quality')='object'
 ) plans ON true
 LEFT JOIN LATERAL (
   WITH source AS (
     SELECT r.*,COALESCE(NULLIF(r.quality_round_id,''),'legacy:'||r.id::text) AS round_key
     FROM eligible_plans p
     CROSS JOIN LATERAL (
       SELECT r.id,r.plan_id,r.status,r.error_message,r.started_at,r.finished_at,r.created_at,
         r.quality_round_id,r.quality_judgment,r.quality_action,r.pelican_config
       FROM scheduled_test_results r WHERE r.plan_id=p.id AND jsonb_typeof(r.pelican_config->'quality')='object'
       ORDER BY r.created_at DESC,r.id DESC LIMIT 6000
     ) r
     WHERE p.account_id=a.id
       AND ($7='all' OR (COALESCE(r.pelican_config->'quality'->>'action_on','')='passed')=($7='recovery'))
       AND ($7='all' OR COALESCE(r.pelican_config->'quality'->>'action','')<>'openai_two_fa_admission')
   ), parsed AS (
     SELECT source.*,COALESCE(pelican_config->>'question_kind','') AS kind,
       COALESCE(pelican_config->>'model_id','') AS model,
       CASE WHEN COALESCE(pelican_config->>'parallel_count','1') ~ '^[1-8]$'
         THEN COALESCE(pelican_config->>'parallel_count','1')::int ELSE 0 END AS parallel,
       CASE WHEN jsonb_typeof(pelican_config->'model_ids')='array' AND pelican_config->'model_ids'<>'[]'::jsonb
         THEN pelican_config->'model_ids' ELSE jsonb_build_array(COALESCE(pelican_config->>'model_id','')) END AS models,
       COALESCE(pelican_config->'quality_batch'->>'route','') AS route,
       CASE WHEN pelican_config->>'question_kind'='state_probe' THEN 'account'
         WHEN pelican_config->>'test_channel'='bps' OR pelican_config->'quality_batch'->>'route'='openai:bps' THEN 'bps'
         ELSE 'account' END AS channel
     FROM source
   ), samples AS (
     SELECT parsed.*,count(*) OVER (PARTITION BY plan_id,round_key,model) AS model_samples,
       -- Explicit BPS observation never falls back; state probes always use
       -- native transport. Old answer snapshots without route evidence cannot
       -- prove native quality: account settings may have changed since then.
       CASE WHEN $6='bps' THEN channel='bps' AND (route='openai:bps' OR pelican_config->>'test_channel'='bps')
         ELSE channel='account' AND (kind='state_probe' OR route=a.platform||':account'
           OR (a.platform<>'openai' AND route LIKE a.platform||':%')) END AS route_known,
       status='success' AND quality_judgment->>'verdict'='correct'
         AND (kind<>'random_reasoning' OR pelican_config->'quality_batch'->>'decision'='passed') AS passed,
       status='failed' AND quality_judgment->>'verdict'='incorrect' AND error_message IN ('answer_mismatch','state_degraded')
         AND (kind<>'random_reasoning' OR (pelican_config->'quality_batch'->>'decision'='peer_passed'
           AND COALESCE(pelican_config->'quality_batch'->>'peer_passed','0') ~ '^[1-9][0-9]*$')) AS failed
     FROM parsed WHERE $7='all' OR channel=$6
   ), rounds AS (
     SELECT max(id) AS id,plan_id,min(started_at) AS started_at,
       (array_agg(model ORDER BY id))[1] AS model_id,
       (array_agg(models ORDER BY id))[1] AS model_ids,
       (array_agg(quality_action ORDER BY id DESC))[1] AS quality_action,
       min(kind) AS question_kind,count(*) AS samples,max(parallel*jsonb_array_length(models)) AS expected_samples,
       count(*) FILTER (WHERE passed) AS passed,count(*) FILTER (WHERE failed) AS failed,
       count(*) FILTER (WHERE status='skipped') AS skipped,
       bool_and(COALESCE(finished_at>=started_at AND quality_action NOT IN ('','stale_run','comparison_pending')
         AND parallel>0 AND jsonb_array_length(models) BETWEEN 1 AND 50 AND parallel*jsonb_array_length(models)<=100
         AND models ? model AND model_samples=parallel AND (status='skipped' OR route_known),false))
         AND count(DISTINCT models)=1 AND count(DISTINCT parallel)=1 AND count(DISTINCT kind)=1 AS complete
     FROM samples GROUP BY plan_id,round_key HAVING bool_or(channel=$6)
     ORDER BY min(started_at) DESC,max(id) DESC LIMIT 60
   ) SELECT jsonb_agg(to_jsonb(rounds) ORDER BY started_at,id) AS items FROM rounds
 ) history ON true
)
SELECT (SELECT count(*) FROM filtered),COALESCE(jsonb_agg(item ORDER BY id DESC),'[]'::jsonb) FROM account_items
`

func (r *scheduledTestResultRepository) ReadQualityOverview(ctx context.Context, filter service.QualityOverviewFilter) ([]service.QualityOverviewAccountEvidence, int64, error) {
	if err := filter.Normalize(); err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var groupID any
	if filter.GroupID != nil {
		groupID = *filter.GroupID
	}
	// Search is literal, so '%' and '_' in account names do not expand the scan.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(filter.Search)
	var raw []byte
	var total int64
	err := r.db.QueryRowContext(ctx, qualityOverviewSQL, filter.Search, "%"+escaped+"%", groupID,
		filter.PageSize, int64(filter.Page-1)*int64(filter.PageSize), filter.TestChannel, filter.ProbeScope).Scan(&total, &raw)
	if err != nil {
		return nil, 0, err
	}
	var items []service.QualityOverviewAccountEvidence
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

var _ service.QualityOverviewRepository = (*scheduledTestResultRepository)(nil)
