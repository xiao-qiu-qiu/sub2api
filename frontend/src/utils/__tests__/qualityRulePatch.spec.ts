import { describe, expect, it } from 'vitest'
import { buildQualityRulePatch, copyQualityRuleConfig, qualityActionPayload, qualityBPSError, qualityBPSForm, qualityBPSPayload, qualityGroupTargets, qualityJudgeConfigured, qualityRuleFieldValue, qualityUsesGroups, type QualityRuleDraft } from '../qualityRulePatch'
import type { QualityBPSPolicy, QualityPolicy, ScheduledTestPlan } from '@/types'

const draft = (): QualityRuleDraft => ({
  model_id: 'new-model', cron_expression: '0 * * * *', enabled: false,
  pelican_config: { question_kind: 'candy', prompt: 'New question', reasoning_effort: 'high', parallel_count: 3,
    quality: { expected_answer: '42', action: 'remove_groups', remove_group_ids: [21], auto_restore: true,
      judge: { group_id: 21, model_id: 'new-judge', prompt: 'Grade the answer' } } },
})
const plan = (): ScheduledTestPlan => ({
  id: 1, account_id: 10, model_id: 'old-model', cron_expression: '*/30 * * * *', enabled: true,
  max_results: 150, auto_recover: false, last_run_at: null, next_run_at: null, created_at: '', updated_at: '',
  pelican_config: { question_kind: 'candy', prompt: 'Own question', reasoning_effort: 'low', parallel_count: 2,
    quality: { expected_answer: '7', action: 'remove_groups', remove_group_ids: [99], auto_restore: false,
      judge: { group_id: 99, model_id: 'own-judge', prompt: 'Own grading instructions' } } },
})
const bps = (): QualityBPSPolicy => ({
  failure_threshold: 2, usage_percent: 0, require_all: false, all_models: false, models: ['gpt-6-astra'],
  omit_unsupported_tools: true, ignore_encrypted_content: true, auto_disable_on_403: false,
  auto_move_on_403: false, target_group_id: -1, session_proxy: false, proxy_source: 'mihomo', cache_creation_as_input: false,
  pass_threshold: 2, hold_on_usage: true, recovery_interval_minutes: 60,
})
const probePlan = () => {
  const rule = plan(), config = rule.pelican_config!
  config.question_kind = 'state_probe'; config.prompt = ''; config.parallel_count = 1
  config.quality!.expected_answer = ''; delete config.quality!.judge
  return rule
}

describe('quality rule copy inputs', () => {
  it('copies recovery intervals and applies them only when restoration is selected', () => {
    const source = draft(), existing = plan()
    Object.assign(source.pelican_config.quality, { recovery_probe_enabled: true, recovery_probe_interval_minutes: 60, recovery_confirmation_minutes: 5 })
    const copy = copyQualityRuleConfig(source.pelican_config)
    expect(copy.quality).toMatchObject({ recovery_probe_enabled: true, recovery_probe_interval_minutes: 60, recovery_confirmation_minutes: 5 })
    const patched = buildQualityRulePatch(existing, source, ['restore'])
    expect(patched.pelican_config!.quality).toMatchObject({ remove_group_ids: [99], auto_restore: true, recovery_probe_enabled: true, recovery_probe_interval_minutes: 60, recovery_confirmation_minutes: 5 })
    Object.assign(existing.pelican_config!.quality!, { auto_restore: true, recovery_probe_enabled: true, recovery_probe_interval_minutes: 120, recovery_confirmation_minutes: 10 })
    expect(buildQualityRulePatch(existing, source, ['test']).pelican_config!.quality).toMatchObject({ recovery_probe_interval_minutes: 120, recovery_confirmation_minutes: 10 })
    source.pelican_config.quality.recovery_probe_enabled = false
    expect(buildQualityRulePatch(existing, source, ['restore']).pelican_config!.quality!.recovery_probe_enabled).toBe(false)
  })
  it('validates recovery intervals and clears recovery probes when switching to observation', () => {
    const policy = { ...draft().pelican_config.quality, recovery_probe_enabled: true, recovery_probe_interval_minutes: 0 }
    expect(() => qualityActionPayload(policy)).toThrow('qualityOps.recoveryIntervalInvalid')
    policy.recovery_probe_interval_minutes = 60
    expect(() => qualityActionPayload({ ...policy, recovery_confirmation_minutes: 1.5 })).toThrow('qualityOps.recoveryIntervalInvalid')
    const observation = qualityActionPayload({ ...policy, action: 'observe_only' })
    expect(observation.auto_restore).toBe(false)
    expect(observation.recovery_probe_enabled).toBeUndefined()
  })
  it('retains Jev choice in copy and bulk patches without importing arbitrary fields', () => {
    const source = draft()
    source.pelican_config.quality.judge = { kind: 'jev', group_id: 0, model_id: '', prompt: 'Compare values' }
    Object.assign(source.pelican_config.quality.judge, { api_key: 'synthetic-secret', base_url: 'https://example.com?key=secret' })
    const copy = copyQualityRuleConfig(source.pelican_config)
    expect(copy.quality.judge).toEqual({ kind: 'jev', group_id: 0, model_id: '', prompt: 'Compare values' })
    expect(qualityJudgeConfigured(copy.quality.judge)).toBe(true)
    const patch = buildQualityRulePatch(plan(), source, ['test'])
    expect(patch.pelican_config!.quality!.judge).toEqual(copy.quality.judge)
    expect(JSON.stringify(patch)).not.toContain('synthetic-secret')
    expect(qualityRuleFieldValue({ ...plan(), pelican_config: copy }, 'test')).not.toEqual(qualityRuleFieldValue(plan(), 'test'))
    expect(qualityJudgeConfigured({ kind: 'jev', group_id: 21, model_id: 'chat', prompt: 'grade' })).toBe(false)
    expect(qualityJudgeConfigured({ group_id: 21, model_id: 'chat', prompt: 'grade' })).toBe(true)
    expect(qualityJudgeConfigured({ kind: 'llm', group_id: 0, model_id: '', prompt: 'grade' })).toBe(false)
  })
  it('accepts null group and BPS model arrays returned by normalized observation and BPS rules', () => {
    const source = plan().pelican_config!
    source.quality!.action = 'enable_bps'
    Object.assign(source.quality!, { remove_group_ids: null, bps: { ...bps(), all_models: true, models: null } })
    const copy = copyQualityRuleConfig(source)
    expect(copy.quality.remove_group_ids).toEqual([])
    expect(copy.quality.bps!.models).toEqual([])
  })

  it('copies editable answer and cooldown settings with independent nested values', () => {
    const source = plan().pelican_config!
    source.model_ids = ['first', 'second']
    source.quality!.action = 'remove_models'
    source.quality!.remove_models = ['first']
    source.quality!.recovery_concurrency = 9
    const original = JSON.stringify(source)
    const copy = copyQualityRuleConfig(source)
    expect(copy).toMatchObject({ prompt: source.prompt, reasoning_effort: 'low', parallel_count: 2, model_ids: ['first', 'second'],
      quality: { action: 'remove_models', remove_models: ['first'], recovery_concurrency: 9, judge: source.quality!.judge } })
    copy.model_ids!.push('third')
    copy.quality.remove_group_ids.push(100)
    copy.quality.remove_models!.push('second')
    copy.quality.judge!.prompt = 'Changed'
    expect(JSON.stringify(source)).toBe(original)
  })

  it('drops generated questions, runtime state and unknown nested fields while preserving reusable random-test inputs', () => {
    const source = plan().pelican_config!
    source.question_kind = 'random_reasoning'
    source.random_question = { id: 'previous-question', family: 'paths', version: 'v1' }
    Object.assign(source, { trigger_source: 'upstream_5xx', model_id: 'result-model', quality_round_id: 'round',
      quality_model_actions: { first: 'cooled' }, quality_model_outcomes: { first: 'failed' },
      quality_restore_diagnostic: { ownership_retained: true }, credentials: 'synthetic-secret' })
    Object.assign(source.quality!, { admission: { enabled: true }, owner_plan_id: 1, failure_count: 3 })
    Object.assign(source.quality!.judge!, { credentials: 'synthetic-secret' })
    const copy = copyQualityRuleConfig(source)
    expect(copy.prompt).toBe('')
    expect(copy.quality.expected_answer).toBe('')
    expect(copy.quality.judge).toEqual({ group_id: 99, model_id: 'own-judge', prompt: 'Own grading instructions' })
    expect(Object.keys(copy).sort()).toEqual(['model_ids', 'parallel_count', 'prompt', 'quality', 'question_kind', 'reasoning_effort', 'rule_name', 'test_channel'])
    for (const key of ['admission', 'owner_plan_id', 'failure_count']) expect(copy.quality).not.toHaveProperty(key)
  })
})

describe('local exact-answer judge configuration', () => {
  it('accepts exact by kind alone while retaining external judge validation', () => {
    expect(qualityJudgeConfigured({ kind: 'exact' } as any)).toBe(true)
    expect(qualityJudgeConfigured({ kind: 'exact', group_id: 0, model_id: '', prompt: '' })).toBe(true)
    expect(qualityJudgeConfigured()).toBe(false)
    expect(qualityJudgeConfigured({ kind: 'jev', group_id: 0, model_id: '', prompt: '' })).toBe(false)
    expect(qualityJudgeConfigured({ kind: 'llm', group_id: 21, model_id: 'judge', prompt: '' })).toBe(false)
  })

  it('copies exact without retaining external judge fields or mutating the source', () => {
    const source = draft().pelican_config
    source.quality.judge = { kind: 'exact', group_id: 21, model_id: 'stale-judge', prompt: 'Stale external instructions' }
    Object.assign(source.quality.judge, { api_key: 'synthetic-secret', base_url: 'https://example.com' })
    const before = JSON.stringify(source), copied = copyQualityRuleConfig(source)
    expect(copied.quality.judge).toEqual({ kind: 'exact', group_id: 0, model_id: '', prompt: '' })
    expect(copied.quality.expected_answer).toBe('42')
    expect(JSON.stringify(source)).toBe(before)
    expect(JSON.stringify(copied)).not.toContain('synthetic-secret')
    copied.quality.judge!.prompt = 'Changed'
    expect(JSON.stringify(source)).toBe(before)
  })

  it('bulk applies exact across different targets while preserving each action and owning independent configs', () => {
    const source = draft(), first = plan(), second = plan()
    source.pelican_config.quality.judge = { kind: 'exact' } as any
    second.pelican_config!.quality!.remove_group_ids = [100]
    const before = JSON.stringify([source, first, second])
    const results = [first, second].map(target => buildQualityRulePatch(target, source, ['test']).pelican_config!)
    for (const [index, result] of results.entries()) {
      expect(result.quality!.judge).toEqual({ kind: 'exact', group_id: 0, model_id: '', prompt: '' })
      expect(result.quality!.expected_answer).toBe('42')
      expect(result.quality!.remove_group_ids).toEqual(index === 0 ? [99] : [100])
    }
    results[0].quality!.judge!.prompt = 'Changed'
    expect(results[1].quality!.judge!.prompt).toBe('')
    expect(JSON.stringify([source, first, second])).toBe(before)
    source.pelican_config.quality.expected_answer = ''
    expect(() => buildQualityRulePatch(first, source, ['test'])).toThrow('qualityOps.questionRequired')
  })

  it('normalizes a target exact judge during action-only patches without importing the draft judge', () => {
    const target = plan()
    target.pelican_config!.quality!.judge = { kind: 'exact', group_id: 21, model_id: 'stale', prompt: 'stale' }
    Object.assign(target.pelican_config!.quality!.judge, { api_key: 'synthetic-secret' })
    const result = buildQualityRulePatch(target, draft(), ['action']).pelican_config!
    expect(result.quality!.judge).toEqual({ kind: 'exact', group_id: 0, model_id: '', prompt: '' })
    expect(result.quality!.expected_answer).toBe('7')
    expect(JSON.stringify(result)).not.toContain('synthetic-secret')
    expect(target.pelican_config!.quality!.judge.model_id).toBe('stale')
  })
})

describe('quality rule partial updates', () => {
  it('bulk switches to generated questions without requiring fixed inputs or changing the judge', () => {
    const input = draft()
    input.pelican_config.question_kind = 'random_reasoning'
    input.pelican_config.prompt = ''
    input.pelican_config.quality.expected_answer = ''
    input.pelican_config.quality.action = 'isolate_models'
    const updated = buildQualityRulePatch(plan(), input, ['test', 'action'])
    expect(updated.pelican_config).toMatchObject({ question_kind: 'random_reasoning', prompt: '', parallel_count: 3,
      quality: { expected_answer: '', action: 'isolate_models', judge: input.pelican_config.quality.judge } })
    expect(updated.pelican_config).not.toHaveProperty('random_question')
  })

  it('sends only selected top-level fields, including an explicit false value', () => {
    expect(buildQualityRulePatch(plan(), draft(), ['model', 'schedule', 'enabled'])).toEqual({
      model_id: 'new-model', cron_expression: '0 * * * *', enabled: false,
    })
  })

  it('replaces the full model selection without overwriting the rule’s grading policy', () => {
    const source = draft()
    Object.assign(source.pelican_config, { model_ids: ['second-model', 'second-model'] })
    const updated = buildQualityRulePatch(plan(), source, ['model'])
    expect(updated.model_id).toBe('new-model')
    expect(updated.pelican_config).toMatchObject({ model_ids: ['new-model', 'second-model'],
      quality: { expected_answer: '7', judge: { model_id: 'own-judge' } } })
  })

  it('preserves different questions, judges and target groups when changing restoration', () => {
    const first = plan(), second = plan()
    second.pelican_config!.quality!.remove_group_ids = [100]
    second.pelican_config!.quality!.judge!.model_id = 'another-judge'
    const before = JSON.stringify([first, second])
    for (const rule of [first, second]) {
      expect(buildQualityRulePatch(rule, draft(), ['restore'])).toEqual({
        pelican_config: { ...rule.pelican_config, quality: { ...rule.pelican_config!.quality, auto_restore: true } },
      })
    }
    expect(JSON.stringify([first, second])).toBe(before)
  })

  it('switches to a probe while preserving each rule’s own action and restoration policy', () => {
    const input = draft(); input.pelican_config.question_kind = 'state_probe'
    const updated = buildQualityRulePatch(plan(), input, ['test'])
    expect(updated.pelican_config).toMatchObject({ question_kind: 'state_probe', prompt: '', parallel_count: 1,
      quality: { expected_answer: '', action: 'remove_groups', remove_group_ids: [99], auto_restore: false } })
    expect(updated.pelican_config!.quality).not.toHaveProperty('judge')
  })

  it('does not convert probe rules when editing only failure actions', () => {
    const rule = plan()
    rule.pelican_config!.question_kind = 'state_probe'
    rule.pelican_config!.prompt = ''; rule.pelican_config!.parallel_count = 1
    rule.pelican_config!.quality!.expected_answer = ''; delete rule.pelican_config!.quality!.judge
    expect(buildQualityRulePatch(rule, draft(), ['action']).pelican_config).toEqual({
      ...rule.pelican_config, quality: { ...rule.pelican_config!.quality, remove_group_ids: [21] },
    })
  })

  it('validates selected fields without requiring untouched grading configuration', () => {
    const input = draft(); delete input.pelican_config.quality.judge
    input.pelican_config.quality.remove_group_ids = []
    expect(buildQualityRulePatch(plan(), input, ['enabled'])).toEqual({ enabled: false })
    expect(() => buildQualityRulePatch(plan(), input, ['test'])).toThrow('qualityOps.configureJudge')
    expect(() => buildQualityRulePatch(plan(), input, ['action'])).toThrow('qualityOps.selectGroups')
    expect(() => buildQualityRulePatch(plan(), input, [])).toThrow('qualityOps.selectBulkFields')
  })

  it('copies and normalizes the BPS policy when enabling BPS on probe rules', () => {
    const input = draft(); input.pelican_config.quality.action = 'enable_bps'
    input.pelican_config.quality.bps = { ...bps(), all_models: true, session_proxy: true, proxy_source: '' }
    const updated = buildQualityRulePatch(probePlan(), input, ['action']).pelican_config!.quality!
    expect(updated).toMatchObject({ action: 'enable_bps', remove_group_ids: [] })
    expect(updated.bps).toEqual({ ...bps(), all_models: true, models: [], target_group_id: 0, session_proxy: true, proxy_source: 'mihomo' })
    expect(input.pelican_config.quality.bps!.models).toEqual(['gpt-6-astra'])
  })

  it('applies the BPS turn-off conditions only together with automatic restoration', () => {
    const own = probePlan(), quality = own.pelican_config!.quality!
    quality.action = 'enable_bps'; quality.bps = { ...bps(), usage_percent: 70, pass_threshold: 4, hold_on_usage: false }
    const input = draft(); input.pelican_config.question_kind = 'state_probe'; input.pelican_config.quality.action = 'enable_bps'
    input.pelican_config.quality.bps = { ...bps(), failure_threshold: 3, pass_threshold: 6, hold_on_usage: true }
    // 只改处理方式：各规则保留自己的关闭条件，原本不开 BPS 的规则用默认值。
    expect(buildQualityRulePatch(own, input, ['action']).pelican_config!.quality!.bps).toMatchObject({ failure_threshold: 3, usage_percent: 0, pass_threshold: 4, hold_on_usage: false })
    expect(buildQualityRulePatch(probePlan(), input, ['action']).pelican_config!.quality!.bps).toMatchObject({ failure_threshold: 3, pass_threshold: 2, hold_on_usage: true })
    expect(buildQualityRulePatch(own, input, ['action', 'restore']).pelican_config!.quality!).toMatchObject({ auto_restore: true, bps: { failure_threshold: 3, pass_threshold: 6, hold_on_usage: true } })
    // 只改自动恢复：只动这两项，开启条件和其它选项不变。
    expect(buildQualityRulePatch(own, input, ['restore']).pelican_config!.quality!).toEqual({ ...quality, auto_restore: true, bps: { ...quality.bps, pass_threshold: 6, hold_on_usage: true } })
    // 自动恢复关掉时次数输入框是隐藏的，不拿它覆盖各规则自己的值，也不因它拦截保存。
    input.pelican_config.quality.auto_restore = false; input.pelican_config.quality.bps.pass_threshold = 0
    expect(buildQualityRulePatch(own, input, ['restore']).pelican_config!.quality!).toEqual({ ...quality, auto_restore: false })
    input.pelican_config.quality.auto_restore = true
    expect(() => buildQualityRulePatch(own, input, ['restore'])).toThrow('qualityOps.bpsPassCountInvalid')
    expect(buildQualityRulePatch(plan(), input, ['restore']).pelican_config!.quality!).not.toHaveProperty('bps')
  })

  it('reads the turn-off count back from saved rules', () => {
    expect(qualityBPSForm(undefined)).toMatchObject({ pass_threshold: 2, hold_on_usage: true })
    expect(qualityBPSForm({ ...bps(), pass_threshold: 5, hold_on_usage: false })).toMatchObject({ pass_threshold: 5, hold_on_usage: false })
    expect(qualityBPSForm({ ...bps(), pass_threshold: 0 }).pass_threshold).toBe(1)
  })

  it('drops BPS settings when changing to another action', () => {
    const rule = probePlan(); rule.pelican_config!.quality!.action = 'enable_bps'; rule.pelican_config!.quality!.bps = bps()
    const input = draft(); input.pelican_config.quality.action = 'disable_scheduling'
    const updated = buildQualityRulePatch(rule, input, ['action']).pelican_config!.quality!
    expect(updated).toMatchObject({ action: 'disable_scheduling', remove_group_ids: [] })
    expect(updated).not.toHaveProperty('bps')
  })

  it('never leaves a candy rule with the enable BPS action', () => {
    const input = draft(); input.pelican_config.quality.action = 'enable_bps'; input.pelican_config.quality.bps = bps()
    expect(() => buildQualityRulePatch(plan(), input, ['action'])).toThrow('qualityOps.bpsRequiresProbe')
    const rule = probePlan(); rule.pelican_config!.quality!.action = 'enable_bps'; rule.pelican_config!.quality!.bps = bps()
    expect(() => buildQualityRulePatch(rule, draft(), ['test'])).toThrow('qualityOps.bpsRequiresProbe')
    input.pelican_config.question_kind = 'state_probe'; input.pelican_config.quality.bps = { ...bps(), failure_threshold: 0 }
    expect(() => buildQualityRulePatch(probePlan(), input, ['action'])).toThrow('qualityOps.bpsTriggerRequired')
  })

  it('converts explicit BPS candy observations without retaining automatic BPS or restoration', () => {
    const rule = probePlan()
    rule.pelican_config!.quality = { ...rule.pelican_config!.quality!, action: 'enable_bps', auto_restore: true, bps: bps() }
    const input = draft(); input.pelican_config.test_channel = 'bps'
    const patch = buildQualityRulePatch(rule, input, ['test', 'restore'])
    expect(patch.pelican_config).toMatchObject({ question_kind: 'candy', test_channel: 'bps', prompt: 'New question',
      quality: { action: 'observe_only', auto_restore: false, remove_group_ids: [], expected_answer: '42', judge: input.pelican_config.quality.judge } })
    expect(patch.pelican_config!.quality).not.toHaveProperty('bps')
    expect(rule.pelican_config!.quality!.action).toBe('enable_bps')
  })

  it('reports the first invalid BPS setting', () => {
    expect(qualityBPSError(bps())).toBe('')
    expect(qualityBPSError({ ...bps(), failure_threshold: 0, usage_percent: 80 })).toBe('')
    expect(qualityBPSError({ ...bps(), failure_threshold: 1.5 })).toBe('qualityOps.bpsCountInvalid')
    expect(qualityBPSError({ ...bps(), failure_threshold: 101 })).toBe('qualityOps.bpsCountInvalid')
    expect(qualityBPSError({ ...bps(), usage_percent: -1 })).toBe('qualityOps.bpsUsageInvalid')
    expect(qualityBPSError({ ...bps(), usage_percent: Number.NaN })).toBe('qualityOps.bpsUsageInvalid')
    expect(qualityBPSError({ ...bps(), failure_threshold: 0 })).toBe('qualityOps.bpsTriggerRequired')
    expect(qualityBPSError({ ...bps(), models: [' '] })).toBe('qualityOps.bpsModelsRequired')
    expect(qualityBPSError({ ...bps(), models: [], all_models: true })).toBe('')
    expect(qualityBPSError({ ...bps(), auto_move_on_403: true })).toBe('qualityOps.bpsTargetGroupRequired')
    expect(qualityBPSError({ ...bps(), auto_move_on_403: true, target_group_id: 0 })).toBe('')
    expect(qualityBPSError({ ...bps(), pass_threshold: 1 })).toBe('')
    expect(qualityBPSError({ ...bps(), pass_threshold: 100 })).toBe('')
    for (const pass_threshold of [0, 101, 1.5, Number.NaN]) expect(qualityBPSError({ ...bps(), pass_threshold })).toBe('qualityOps.bpsPassCountInvalid')
  })
})

describe('BPS recovery interval in quality rules', () => {
  it('defaults legacy rules to 60 minutes and persists custom intervals', () => {
    expect(qualityBPSForm({}).recovery_interval_minutes).toBe(60)
    expect(qualityBPSPayload({ ...bps(), recovery_interval_minutes: undefined }).recovery_interval_minutes).toBe(60)
    for (const minutes of [1, 30, 360, 10080]) {
      const form = qualityBPSForm({ ...bps(), recovery_interval_minutes: minutes })
      expect(qualityBPSError(form)).toBe('')
      expect(qualityBPSPayload(form).recovery_interval_minutes).toBe(minutes)
    }
  })
  it.each([0, -1, 1.5, 10081, NaN, Infinity])('rejects invalid interval %s', (minutes) => {
    expect(qualityBPSError({ ...bps(), recovery_interval_minutes: minutes })).toBe('admin.accounts.openai.excelBPS403RecoveryIntervalInvalid')
  })
})

describe('composed model isolation', () => {
  it('uses tested models and selected groups without legacy cooldown or concurrency fields', () => {
    const input = draft()
    input.pelican_config.quality.action = 'isolate_models'
    input.pelican_config.quality.remove_models = ['stale-target']
    input.pelican_config.quality.recovery_concurrency = 2
    input.pelican_config.model_ids = ['sol', 'astra']
    const patch = buildQualityRulePatch(plan(), input, ['action', 'model'])
    expect(patch.pelican_config).toMatchObject({ model_ids: ['new-model', 'sol', 'astra'], quality: {
      action: 'isolate_models', remove_group_ids: [21], expected_answer: '7', auto_restore: false,
    } })
    expect(patch.pelican_config!.quality).not.toHaveProperty('remove_models')
    expect(patch.pelican_config!.quality).not.toHaveProperty('recovery_concurrency')
    expect(patch.pelican_config!.quality).not.toHaveProperty('bps')
  })

  it('requires explicit groups and per-model answer tests for bulk changes', () => {
    const input = draft()
    input.pelican_config.quality.action = 'isolate_models'
    input.pelican_config.quality.remove_group_ids = []
    expect(() => buildQualityRulePatch(plan(), input, ['action'])).toThrow('qualityOps.selectGroups')
    input.pelican_config.quality.remove_group_ids = [21]
    expect(() => buildQualityRulePatch(probePlan(), input, ['action'])).toThrow('qualityOps.isolationRequiresCandy')
  })
})


describe('move group quality rules', () => {
  it('builds an independent single-rule payload and clears settings from other actions', () => {
    const quality: QualityPolicy = { ...draft().pelican_config.quality, action: 'move_groups', move_group_ids: [1, 101],
      remove_models: ['old-model'], recovery_concurrency: 2, bps: bps() }
    const original = JSON.stringify(quality)
    const payload = qualityActionPayload(quality)
    expect(payload).toMatchObject({ action: 'move_groups', move_group_ids: [1, 101], remove_group_ids: [], auto_restore: true })
    for (const field of ['remove_models', 'recovery_concurrency', 'bps']) expect(payload).not.toHaveProperty(field)
    payload.move_group_ids!.push(102)
    expect(JSON.stringify(quality)).toBe(original)
  })

  it('accepts up to 100 target groups without limiting the IDs to 100', () => {
    const targets = Array.from({ length: 100 }, (_, index) => index + 1001)
    const quality: QualityPolicy = { ...draft().pelican_config.quality, action: 'move_groups', move_group_ids: targets }
    expect(qualityActionPayload(quality).move_group_ids).toEqual(targets)
    expect(() => qualityActionPayload({ ...quality, move_group_ids: [...targets, 2001] })).toThrow('qualityOps.invalidMoveGroups')
  })

  it.each([undefined, null, []])('requires targets instead of falling back to removal groups (%j)', (targets) => {
    const quality: QualityPolicy = { ...draft().pelican_config.quality, action: 'move_groups' }
    Object.assign(quality, { move_group_ids: targets })
    expect(() => qualityActionPayload(quality)).toThrow('qualityOps.selectMoveGroups')
  })

  it.each([
    { name: 'duplicates', targets: [21, 21] },
    { name: 'zero', targets: [0] },
    { name: 'negative IDs', targets: [-1] },
    { name: 'fractions', targets: [1.5] },
    { name: 'NaN', targets: [NaN] },
    { name: 'infinity', targets: [Infinity] },
    { name: 'unsafe integers', targets: [Number.MAX_SAFE_INTEGER + 1] },
    { name: 'string IDs', targets: ['21'] },
    { name: 'sparse arrays', targets: new Array<number>(2) },
    { name: 'non-array targets', targets: '21' },
  ])('rejects $name without silently correcting the targets', ({ targets }) => {
    const quality: QualityPolicy = { ...draft().pelican_config.quality, action: 'move_groups' }
    Object.assign(quality, { move_group_ids: targets })
    expect(() => qualityActionPayload(quality)).toThrow('qualityOps.invalidMoveGroups')
  })

  it('copies reusable move targets for account rules and presets without sharing the array or runtime state', () => {
    const source = draft().pelican_config
    source.quality.action = 'move_groups'; source.quality.move_group_ids = [101, 202]
    Object.assign(source.quality, { original_groups: [21], added_target_groups: [101], owner_plan_id: 7 })
    const copy = copyQualityRuleConfig(source)
    expect(copy.quality).toMatchObject({ action: 'move_groups', move_group_ids: [101, 202], remove_group_ids: [], auto_restore: true })
    for (const field of ['original_groups', 'added_target_groups', 'owner_plan_id']) expect(copy.quality).not.toHaveProperty(field)
    copy.quality.move_group_ids!.push(303)
    expect(source.quality.move_group_ids).toEqual([101, 202])
  })

  it.each<QualityPolicy['action']>(['remove_groups', 'isolate_models', 'remove_models', 'disable_scheduling', 'enable_bps', 'observe_only'])(
    'clears stale move targets when using %s in copies, single payloads and bulk action patches', (action) => {
      const source = draft()
      Object.assign(source.pelican_config.quality, { action, move_group_ids: [101], remove_models: ['old-model'], bps: bps() })
      const own = probePlan()
      own.pelican_config!.quality!.action = 'move_groups'; own.pelican_config!.quality!.move_group_ids = [202]
      if (action === 'isolate_models') own.pelican_config!.question_kind = 'candy'
      expect(copyQualityRuleConfig(source.pelican_config).quality).not.toHaveProperty('move_group_ids')
      expect(qualityActionPayload(source.pelican_config.quality)).not.toHaveProperty('move_group_ids')
      expect(buildQualityRulePatch(own, source, ['action']).pelican_config!.quality).not.toHaveProperty('move_group_ids')
      expect(source.pelican_config.quality.move_group_ids).toEqual([101])
    },
  )

  it('applies shared move targets to each bulk rule while preserving its own question, judge and restoration policy', () => {
    const first = plan(), second = plan(), source = draft()
    second.pelican_config!.quality!.judge!.model_id = 'second-judge'
    second.pelican_config!.quality!.expected_answer = 'second-answer'
    second.pelican_config!.quality!.auto_restore = true
    source.pelican_config.quality.action = 'move_groups'; source.pelican_config.quality.move_group_ids = [101, 202]
    const before = JSON.stringify([first, second, source])
    for (const own of [first, second]) {
      const updated = buildQualityRulePatch(own, source, ['action']).pelican_config!
      expect(updated).toEqual({ ...own.pelican_config, quality: { ...own.pelican_config!.quality!,
        action: 'move_groups', remove_group_ids: [], move_group_ids: [101, 202] } })
      updated.quality!.move_group_ids!.push(303)
    }
    expect(JSON.stringify([first, second, source])).toBe(before)
  })

  it('validates move targets during bulk action updates', () => {
    const source = draft()
    source.pelican_config.quality.action = 'move_groups'
    expect(() => buildQualityRulePatch(plan(), source, ['action'])).toThrow('qualityOps.selectMoveGroups')
    source.pelican_config.quality.move_group_ids = [21, 21]
    expect(() => buildQualityRulePatch(plan(), source, ['action'])).toThrow('qualityOps.invalidMoveGroups')
    source.pelican_config.quality.move_group_ids = [21]
    expect(buildQualityRulePatch(plan(), source, ['action', 'restore']).pelican_config!.quality).toMatchObject({ auto_restore: true, move_group_ids: [21] })
  })

  it('preserves each rule’s own move targets when only restoration is selected', () => {
    for (const targets of [[101], [202, 303]]) {
      const own = plan()
      Object.assign(own.pelican_config!.quality!, { action: 'move_groups', remove_group_ids: [], move_group_ids: targets })
      expect(buildQualityRulePatch(own, draft(), ['restore']).pelican_config!.quality).toEqual({ ...own.pelican_config!.quality!, auto_restore: true })
    }
  })

  it('clears stale targets but preserves explicit move actions when switching to BPS', () => {
    const own = plan()
    own.pelican_config!.quality!.move_group_ids = [101]
    expect(buildQualityRulePatch(own, draft(), ['restore']).pelican_config!.quality).not.toHaveProperty('move_group_ids')
    own.pelican_config!.quality!.action = 'move_groups'
    const source = draft(); source.pelican_config.test_channel = 'bps'
    const updated = buildQualityRulePatch(own, source, ['test', 'restore']).pelican_config!.quality!
    expect(updated).toMatchObject({ action: 'move_groups', auto_restore: true, move_group_ids: [101] })
    expect(own.pelican_config!.quality!.move_group_ids).toEqual([101])
  })

  it('includes active move targets in action diffs without treating them as restoration or test changes', () => {
    const first = plan(), second = plan()
    Object.assign(first.pelican_config!.quality!, { action: 'move_groups', remove_group_ids: [], move_group_ids: [101] })
    Object.assign(second.pelican_config!.quality!, { action: 'move_groups', remove_group_ids: [], move_group_ids: [202] })
    expect(qualityRuleFieldValue(first, 'action')).not.toEqual(qualityRuleFieldValue(second, 'action'))
    for (const field of ['restore', 'test'] as const) expect(qualityRuleFieldValue(first, field)).toEqual(qualityRuleFieldValue(second, field))
    first.pelican_config!.quality!.action = 'observe_only'; second.pelican_config!.quality!.action = 'observe_only'
    expect(qualityRuleFieldValue(first, 'action')).toEqual(qualityRuleFieldValue(second, 'action'))
  })

  it('provides display targets while preserving the removal-only meaning of qualityUsesGroups', () => {
    const quality = draft().pelican_config.quality
    expect(qualityGroupTargets()).toEqual([])
    for (const action of ['remove_groups', 'isolate_models'] as const) {
      expect(qualityUsesGroups(action)).toBe(true)
      expect(qualityGroupTargets({ ...quality, action, move_group_ids: [101] })).toEqual([21])
    }
    expect(qualityUsesGroups('move_groups')).toBe(false)
    expect(qualityGroupTargets({ ...quality, action: 'move_groups', move_group_ids: [101, 202] })).toEqual([101, 202])
    expect(qualityGroupTargets({ ...quality, action: 'move_groups' })).toEqual([])
    expect(qualityGroupTargets({ ...quality, action: 'observe_only', move_group_ids: [101] })).toEqual([])
  })
})

describe('complete template selected fields', () => {
  it('imports retention and 5xx trigger only with their explicit bulk checkboxes', () => {
    const source = { ...draft(), max_results: 173 }
    source.pelican_config.quality.trigger_on_upstream_5xx = true
    const untouched = buildQualityRulePatch(plan(), source, ['enabled'])
    expect(untouched).toEqual({ enabled: false })
    const retention = buildQualityRulePatch(plan(), source, ['retention'])
    expect(retention).toEqual({ max_results: 173 })
    const triggered = buildQualityRulePatch(plan(), source, ['trigger'])
    expect(triggered.pelican_config!.quality!.trigger_on_upstream_5xx).toBe(true)
    expect(triggered.pelican_config!.quality!.action).toBe(plan().pelican_config!.quality!.action)
    expect(triggered).not.toHaveProperty('max_results')
  })
  it('rejects retention too small for the targets existing samples when only retention is selected', () => {
    const target = plan(); target.pelican_config!.model_ids = ['first', 'second']; target.pelican_config!.parallel_count = 3
    expect(() => buildQualityRulePatch(target, { ...draft(), max_results: 5 }, ['retention'])).toThrow('qualityOps.retentionTooSmall')
    expect(buildQualityRulePatch(target, { ...draft(), max_results: 6 }, ['retention'])).toEqual({ max_results: 6 })
  })
  it('never inherits a punitive action for old presets and removes hidden restrictions', () => {
    const source = draft().pelican_config
    delete (source.quality as any).action
    const copied = copyQualityRuleConfig(source)
    expect(copied.quality.action).toBe('observe_only'); expect(copied.quality.remove_group_ids).toEqual([]); expect(copied.quality.auto_restore).toBe(false)
    expect(copied.quality.remove_models).toBeUndefined(); expect(copied.quality.bps).toBeUndefined()
  })
})


describe('independent recovery rule payloads', () => {
  const healthy = (): QualityPolicy => ({ action_on: 'passed', action: 'move_groups', move_group_ids: [7], remove_group_ids: [], auto_restore: false, expected_answer: '' })
  it('uses the 60+5 defaults without requiring old quarantine ownership or auto restore', () => {
    expect(qualityActionPayload(healthy())).toMatchObject({ action_on: 'passed', auto_restore: false, recovery_probe_enabled: true, recovery_probe_interval_minutes: 60, recovery_confirmation_minutes: 5 })
    expect(qualityActionPayload({ ...healthy(), action: 'observe_only' }).recovery_probe_enabled).toBe(true)
    expect(qualityActionPayload({ ...healthy(), action: 'enable_scheduling' }).action).toBe('enable_scheduling')
  })
  it('rejects failure-only actions and invalid recovery intervals', () => {
    expect(() => qualityActionPayload({ ...healthy(), action: 'disable_scheduling' })).toThrow('qualityOps.invalidHealthyAction')
    expect(() => qualityActionPayload({ ...healthy(), recovery_confirmation_minutes: 0 })).toThrow('qualityOps.recoveryIntervalInvalid')
    expect(() => qualityActionPayload({ ...healthy(), action_on: 'failed', action: 'enable_scheduling' })).toThrow('qualityOps.invalidHealthyAction')
  })
  it('copies the recovery scope and changes only its interval for a schedule-only bulk edit', () => {
    const target = plan(), source = draft()
    target.pelican_config!.quality = { ...healthy(), recovery_probe_interval_minutes: 60, recovery_confirmation_minutes: 5 }
    source.pelican_config.quality = { ...healthy(), recovery_probe_interval_minutes: 120, recovery_confirmation_minutes: 10 }
    expect(copyQualityRuleConfig(source.pelican_config).quality.action_on).toBe('passed')
    const result = buildQualityRulePatch(target, source, ['schedule'])
    expect(result.pelican_config?.quality).toMatchObject({ action_on: 'passed', recovery_probe_interval_minutes: 120, recovery_confirmation_minutes: 5 })
  })
})
