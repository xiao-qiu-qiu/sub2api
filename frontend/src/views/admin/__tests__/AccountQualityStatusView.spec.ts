import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountQualityStatusView from '../AccountQualityStatusView.vue'
import { getQualityOverview, type QualityOverviewAccount, type QualityUnknownReason } from '@/api/admin/qualityOverview'
import { runQualityPlan } from '@/api/admin/accountQuality'
import zhQualityOps from '@/i18n/locales/zh/qualityOps'

const { push } = vi.hoisted(() => ({ push: vi.fn() }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push }), RouterLink: { props: ['to'], template: '<a :data-to="JSON.stringify(to)"><slot /></a>' } }))
vi.mock('vue-i18n', async () => {
  const { default: messages } = await import('@/i18n/locales/zh/qualityOps')
  const t = (key: string, params: Record<string, string | number> = {}) => {
    if (key === 'qualityOps.statusEvaluatedRounds') {
      return Object.entries(params).reduce((message, [name, value]) => message.replace(`{${name}}`, String(value)), messages.statusEvaluatedRounds)
    }
    if (key.startsWith('qualityOps.statusReasons.')) return messages.statusReasons[key.slice('qualityOps.statusReasons.'.length) as QualityUnknownReason] ?? key
    if (key.startsWith('qualityOps.statusReasonHelp.')) return messages.statusReasonHelp[key.slice('qualityOps.statusReasonHelp.'.length) as QualityUnknownReason] ?? key
    return key
  }
  return { useI18n: () => ({ t, te: () => true }) }
})
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 1 } }) }))
vi.mock('@/api/admin/qualityOverview', () => ({ getQualityOverview: vi.fn() }))
vi.mock('@/api/admin/accountQuality', () => ({ runQualityPlan: vi.fn() }))
vi.mock('@/api/admin/groups', () => ({ getAllIncludingInactive: vi.fn().mockResolvedValue([{ id: 7, name: 'Pool' }]) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('@/components/admin/operations/QualityTabs.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('@/components/common/BaseDialog.vue', () => ({ default: { props: ['show'], template: '<div v-if="show" role="dialog"><slot /></div>' } }))
const fixture = (): QualityOverviewAccount => ({
  account_id: 3, account_name: 'Account three', platform: 'openai', type: 'oauth', groups: [], status: 'unknown', healthy_rate: 50,
  history: (['healthy', 'degraded', 'unknown'] as const).map((status, i) => ({ id: i + 1, plan_id: 9, started_at: '2026-11-06T00:00:00Z', status, model_id: 'test-model', question_kind: 'state_probe', quality_action: 'inconclusive' })),
  plans: [{ id: 9, model_id: 'test-model', enabled: true, running_until: null, next_run_at: null, action: 'move_groups', auto_restore: true }],
})
const response = (items = [fixture()]) => ({ items, total: items.length, page: 1, page_size: 20 })
const mockAccounts = (...items: QualityOverviewAccount[]) => {
  vi.mocked(getQualityOverview).mockImplementation(async params => response(params.probe_scope === 'recovery' ? [] : items))
}
const reasonCodes = [
  'stale_result', 'quota_exhausted', 'rate_limited', 'route_unverified', 'comparison_pending',
  'incomplete_round', 'probe_failed', 'skipped', 'inconclusive',
] as const satisfies readonly QualityUnknownReason[]
let wrapper: ReturnType<typeof mount>
beforeEach(() => { vi.clearAllMocks(); vi.mocked(getQualityOverview).mockReset().mockImplementation(async params => response(params.probe_scope === 'recovery' ? [] : [fixture()])); vi.mocked(runQualityPlan).mockReset().mockResolvedValue() })
afterEach(() => { wrapper?.unmount(); vi.useRealTimers() })

describe('account quality status', () => {
  it('labels BPS scopes and opens matching BPS creation and recovery presets', async () => {
    vi.mocked(getQualityOverview).mockResolvedValue(response([]))
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    await wrapper.get('.channel-switch button:last-child').trigger('click'); await flushPromises()
    const main = wrapper.get('[data-probe-scope="main"]'), recovery = wrapper.get('[data-probe-scope="recovery"]')
    expect(main.get('h2').text()).toBe('qualityOps.bpsProbeScope')
    expect(main.get('.empty-state').text()).toContain('qualityOps.noBpsProbeScope')
    expect(JSON.parse(main.get('.scope-configure').attributes('data-to')).query).toEqual({ create: '1', channel: 'bps' })
    expect(recovery.get('h2').text()).toBe('qualityOps.bpsRecoveryScope')
    expect(recovery.get('.empty-state').text()).toContain('qualityOps.noBpsRecoveryScope')
    expect(JSON.parse(recovery.get('.scope-configure').attributes('data-to')).query).toEqual({ recovery: '1', channel: 'bps' })
  })
  it('explains missing historical route evidence without displaying it as a failed answer', async () => {
    const account = fixture()
    account.history = [{ ...account.history[2], route_evidence_missing: true }]
    vi.mocked(getQualityOverview).mockImplementation(async params => response(params.probe_scope === 'recovery' ? [] : [account]))
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    await wrapper.get('button.bar-unknown').trigger('click')
    expect(wrapper.get('[data-testid="quality-missing-route"]').text()).toBe('qualityOps.statusMissingRouteEvidence')
    expect(wrapper.findAll('button.bar-degraded')).toHaveLength(0)
  })
  it('loads both scopes with independent pagination and runs the recovery rule in its own section', async () => {
    vi.mocked(getQualityOverview).mockImplementation(async params => {
      const account = fixture()
      if (params.probe_scope === 'recovery') {
        account.account_name = 'Recovery account'
        account.plans = [{ ...account.plans[0], id: 19, action_on: 'passed' }]
      }
      return { ...response([account]), total: params.probe_scope === 'main' ? 21 : 1, page: params.page }
    })
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    const main = wrapper.get('[data-probe-scope="main"]'), recovery = wrapper.get('[data-probe-scope="recovery"]')
    expect(main.text()).not.toContain('Recovery account')
    expect(recovery.text()).toContain('Recovery account')
    await recovery.get('.probe-button').trigger('click'); await flushPromises()
    expect(runQualityPlan).toHaveBeenLastCalledWith(19)
    vi.mocked(getQualityOverview).mockClear()
    await main.get('.list-footer button:last-child').trigger('click'); await flushPromises()
    expect(getQualityOverview).toHaveBeenCalledTimes(1)
    expect(getQualityOverview).toHaveBeenCalledWith(expect.objectContaining({ probe_scope: 'main', page: 2 }), expect.any(AbortSignal))
  })
  it('renders separate healthy, degraded and inconclusive bars and opens evidence', async () => {
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.findAll('button.bar-healthy')).toHaveLength(1)
    expect(wrapper.findAll('button.bar-degraded')).toHaveLength(1)
    expect(wrapper.findAll('button.bar-unknown')).toHaveLength(1)
    expect(wrapper.findAll('.bar-empty')).toHaveLength(57)
    expect(wrapper.get('.rate-cell strong').text()).toBe('50%')
    expect(wrapper.get('.account-meta').text()).toContain('qualityOps.statusUnknown')
    await wrapper.get('button.bar-degraded').trigger('click')
    expect(wrapper.get('[role="dialog"]').text()).toContain('qualityOps.statusDegraded')
  })
  it.each(reasonCodes)('explains %s in the latest status, history accessibility labels and evidence', async reason => {
    const account = fixture()
    account.history = [{ ...account.history[2], reason_codes: [reason] }]
    account.healthy_rate = null
    mockAccounts(account)
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    const row = wrapper.get('[data-account-id="3"]')
    const reasonLabel = zhQualityOps.statusReasons[reason]
    expect(row.get('.account-meta .state-label').text()).toBe(reasonLabel)
    const bar = row.get('button.bar-unknown')
    expect(bar.attributes('title')).toContain(reasonLabel)
    expect(bar.attributes('aria-label')).toBe(bar.attributes('title'))
    expect(row.findAll('button.bar-degraded')).toHaveLength(0)
    await bar.trigger('click')
    const reasons = wrapper.findAll('[data-testid="quality-reason"]')
    expect(reasons).toHaveLength(1)
    expect(reasons[0].text()).toContain(reasonLabel)
    const help = reason === 'route_unverified' ? 'qualityOps.statusMissingRouteEvidence' : zhQualityOps.statusReasonHelp[reason]
    expect(reasons[0].text()).toContain(help)
    if (reason === 'route_unverified') {
      expect(reasons[0].get('p[data-testid="quality-missing-route"]').text()).toBe(help)
    }
    expect(wrapper.get('.evidence-reasons').attributes('aria-label')).toBe('qualityOps.statusSampleReason')
  })
  it('uses the first supplied reason for the latest status and explains every reason separately', async () => {
    const account = fixture()
    const reasons: QualityUnknownReason[] = ['rate_limited', 'quota_exhausted', 'stale_result']
    account.history = [
      { ...account.history[2], id: 1, reason_codes: ['probe_failed'] },
      { ...account.history[2], id: 2, reason_codes: reasons },
    ]
    account.healthy_rate = null
    mockAccounts(account)
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    const row = wrapper.get('[data-account-id="3"]')
    expect.soft(row.get('.account-meta .state-label').text()).toBe(zhQualityOps.statusReasons.rate_limited)
    const bar = row.findAll('button.bar-unknown')[1]
    for (const reason of reasons) {
      expect(bar.attributes('title')).toContain(zhQualityOps.statusReasons[reason])
      expect(bar.attributes('aria-label')).toContain(zhQualityOps.statusReasons[reason])
    }
    await bar.trigger('click')
    const cards = wrapper.findAll('[data-testid="quality-reason"]')
    expect(cards).toHaveLength(reasons.length)
    for (const reason of reasons) {
      const card = cards.find(item => item.get('strong').text() === zhQualityOps.statusReasons[reason])
      expect(card, `Missing reason card: ${reason}`).toBeDefined()
      expect(card!.text()).toContain(zhQualityOps.statusReasonHelp[reason])
    }
  })
  it.each([undefined, []] as (QualityUnknownReason[] | undefined)[])('keeps unknown generic without explicit reasons (%j)', async reasonCodes => {
    const account = fixture()
    account.history[2].reason_codes = reasonCodes
    mockAccounts(account)
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('.account-meta .state-label').text()).toBe('qualityOps.statusUnknown')
    const bar = wrapper.get('button.bar-unknown')
    expect(bar.attributes('title')).toContain('qualityOps.statusUnknown')
    expect(bar.attributes('aria-label')).toContain('qualityOps.statusUnknown')
  })
  it.each([
    { action: 'stale_run', reason: 'stale_result' },
    { action: 'comparison_pending', reason: 'comparison_pending' },
  ] as const)('infers $reason from legacy $action without reason codes', async ({ action, reason }) => {
    const account = fixture()
    account.history[2].quality_action = action
    mockAccounts(account)
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('.account-meta .state-label').text()).toBe(zhQualityOps.statusReasons[reason])
    await wrapper.get('button.bar-unknown').trigger('click')
    expect(wrapper.get('[data-testid="quality-reason"]').text()).toContain(zhQualityOps.statusReasonHelp[reason])
  })
  it('infers an unverified legacy route inside its reason card', async () => {
    const account = fixture()
    account.history[2].route_evidence_missing = true
    mockAccounts(account)
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('.account-meta .state-label').text()).toBe(zhQualityOps.statusReasons.route_unverified)
    await wrapper.get('button.bar-unknown').trigger('click')
    expect(wrapper.get('[data-testid="quality-reason"] p[data-testid="quality-missing-route"]').text()).toBe('qualityOps.statusMissingRouteEvidence')
  })
  it('shows conclusive coverage from the API and labels the rate', async () => {
    mockAccounts({ ...fixture(), evaluated_rounds: 2, total_rounds: 3 })
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    const rate = wrapper.get('.rate-cell')
    expect(rate.get('strong').text()).toBe('50%')
    expect(rate.text()).toContain('qualityOps.statusHealthyRate')
    expect(rate.attributes('title')).toBe('qualityOps.statusRateHelp')
    expect(rate.get('.rate-coverage[data-testid="quality-evaluated-rounds"]').text()).toBe('有效 2/3 轮')
  })
  it('derives legacy coverage from actual history and excludes 57 placeholder slots', async () => {
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('[data-testid="quality-evaluated-rounds"]').text()).toBe('有效 2/3 轮')
    expect(wrapper.get('.rate-cell strong').text()).toBe('50%')
    const placeholders = wrapper.findAll('.bar-empty')
    expect(placeholders).toHaveLength(57)
    for (const slot of placeholders) expect(slot.attributes('title')).toBe('qualityOps.statusEmptySlot')
    for (const status of ['healthy', 'degraded'] as const) {
      const bar = wrapper.get(`button.bar-${status}`)
      expect(bar.attributes('title')).toContain(status === 'healthy' ? 'qualityOps.statusHealthy' : 'qualityOps.statusDegraded')
      expect(bar.attributes('aria-label')).toBe(bar.attributes('title'))
    }
  })
  it.each([
    { fields: { evaluated_rounds: 1 }, expected: '有效 1/3 轮' },
    { fields: { total_rounds: 4 }, expected: '有效 2/4 轮' },
    { fields: { evaluated_rounds: 0, total_rounds: 0 }, expected: '有效 0/0 轮' },
  ])('falls back only for missing counters and preserves explicit zero ($expected)', async ({ fields, expected }) => {
    mockAccounts({ ...fixture(), ...fields })
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('[data-testid="quality-evaluated-rounds"]').text()).toBe(expected)
  })
  it('keeps coverage visible when every actual round is inconclusive', async () => {
    const account = fixture()
    account.history = account.history.map(point => ({ ...point, status: 'unknown', reason_codes: ['quota_exhausted'] }))
    account.healthy_rate = null
    mockAccounts(account)
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('.rate-cell strong').text()).toBe('—')
    expect(wrapper.get('[data-testid="quality-evaluated-rounds"]').text()).toBe('有效 0/3 轮')
    expect(wrapper.findAll('button.bar-unknown')).toHaveLength(3)
  })
  it('keeps zero coverage visible for an untested account without history', async () => {
    mockAccounts({ ...fixture(), history: [], plans: [], status: 'untested', healthy_rate: null })
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('.rate-cell strong').text()).toBe('—')
    expect(wrapper.get('[data-testid="quality-evaluated-rounds"]').text()).toBe('有效 0/0 轮')
    expect(wrapper.findAll('.bar-empty')).toHaveLength(60)
  })
  it('does not turn accounts without history into healthy accounts and links to creation', async () => {
    const account = { ...fixture(), history: [], plans: [], status: 'untested' as const, healthy_rate: null }
    vi.mocked(getQualityOverview).mockResolvedValue(response([account]))
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('.rate-cell strong').text()).toBe('—')
    expect(wrapper.get('.probe-button').attributes('disabled')).toBeDefined()
    await wrapper.get('.rule-button').trigger('click')
    expect(push).toHaveBeenCalledWith({ path: '/admin/account-quality', query: { create_account: 3 } })
  })
  it('queues the existing rule and links to its editor', async () => {
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    await wrapper.get('.probe-button').trigger('click'); await flushPromises()
    expect(runQualityPlan).toHaveBeenCalledTimes(1)
    expect(runQualityPlan).toHaveBeenCalledWith(9)
    expect(wrapper.get('[role="status"]').text()).toContain('qualityOps.statusProbeQueued')
    await wrapper.get('.rule-button').trigger('click')
    expect(push).toHaveBeenCalledWith({ path: '/admin/account-quality', query: { plan_id: 9, edit: '1' } })
  })
  it('ignores stale requests when the channel changes and resumes polling after search', async () => {
    vi.useFakeTimers()
    let resolveOld!: (value: ReturnType<typeof response>) => void
    vi.mocked(getQualityOverview).mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve }))
    wrapper = mount(AccountQualityStatusView)
    await wrapper.get('.channel-switch button:last-child').trigger('click'); await flushPromises()
    resolveOld(response([{ ...fixture(), account_name: 'Stale result' }]))
    await flushPromises()
    expect(wrapper.text()).not.toContain('Stale result')
    expect(getQualityOverview).toHaveBeenLastCalledWith(expect.objectContaining({ test_channel: 'bps' }), expect.any(AbortSignal))
    await wrapper.get('.search-field input').setValue('abc')
    await vi.advanceTimersByTimeAsync(300); await flushPromises()
    expect(getQualityOverview).toHaveBeenLastCalledWith(expect.objectContaining({ search: 'abc', page: 1 }), expect.any(AbortSignal))
    const calls = vi.mocked(getQualityOverview).mock.calls.length
    await vi.advanceTimersByTimeAsync(30_000); await flushPromises()
    expect(vi.mocked(getQualityOverview).mock.calls.length).toBeGreaterThan(calls)
  })
  it('keeps failed fetches retryable and reports a failed manual probe', async () => {
    vi.mocked(getQualityOverview).mockRejectedValueOnce(new Error('Unavailable'))
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('Unavailable')
    await wrapper.get('[role="alert"] button').trigger('click'); await flushPromises()
    vi.mocked(runQualityPlan).mockRejectedValueOnce(new Error('Queue unavailable'))
    await wrapper.get('.probe-button').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('Queue unavailable')
    expect(wrapper.get('.probe-button').attributes('disabled')).toBeUndefined()
  })
  it('runs only a rule for the selected channel and prefers the original quarantine rule', async () => {
    const account = fixture(), original = account.plans[0]
    account.plans = [{ ...original, id: 1, action: 'observe_only' }, { ...original, id: 2, action: 'observe_only', test_channel: 'bps' }, original]
    vi.mocked(getQualityOverview).mockResolvedValue(response([account]))
    wrapper = mount(AccountQualityStatusView); await flushPromises()
    await wrapper.get('.probe-button').trigger('click'); await flushPromises()
    expect(runQualityPlan).toHaveBeenLastCalledWith(9)
    await wrapper.get('.channel-switch button:last-child').trigger('click'); await flushPromises()
    await wrapper.get('.probe-button').trigger('click'); await flushPromises()
    expect(runQualityPlan).toHaveBeenLastCalledWith(2)
  })
})
