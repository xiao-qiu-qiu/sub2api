import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountQualityStatusView from '../AccountQualityStatusView.vue'
import { getQualityOverview, type QualityOverviewAccount } from '@/api/admin/qualityOverview'
import { runQualityPlan } from '@/api/admin/accountQuality'

const { push } = vi.hoisted(() => ({ push: vi.fn() }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push }), RouterLink: { props: ['to'], template: '<a><slot /></a>' } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, te: () => true }) }))
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
  history: ['healthy', 'degraded', 'unknown'].map((status, i) => ({ id: i + 1, plan_id: 9, started_at: '2026-11-06T00:00:00Z', status: status as 'healthy', model_id: 'test-model', question_kind: 'state_probe', quality_action: 'inconclusive' })),
  plans: [{ id: 9, model_id: 'test-model', enabled: true, running_until: null, next_run_at: null, action: 'move_groups', auto_restore: true }],
})
const response = (items = [fixture()]) => ({ items, total: items.length, page: 1, page_size: 20 })
let wrapper: ReturnType<typeof mount>
beforeEach(() => { vi.clearAllMocks(); vi.mocked(getQualityOverview).mockReset().mockResolvedValue(response()); vi.mocked(runQualityPlan).mockReset().mockResolvedValue() })
afterEach(() => { wrapper?.unmount(); vi.useRealTimers() })

describe('account quality status', () => {
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
