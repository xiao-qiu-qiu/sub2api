import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountRotationBadge from '../AccountRotationBadge.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: { count?: number }) => values?.count ? `${key} ${values.count}` : key }) }))
const now = 1_800_000_000
const state: NonNullable<Account['account_rotation']> = {
  phase: 'resting', reason: 'manual', hold: true, waiting_count: 3,
  started_at: now - 300, drain_at: now + 18000, rest_until: now + 60, idle_since: now, rest_seconds: 60,
}
afterEach(() => vi.useRealTimers())

describe('AccountRotationBadge', () => {
  it('shows the manual rest, live countdown and queued request count', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(now * 1000)
    const wrapper = mount(AccountRotationBadge, { props: { state } })
    expect(wrapper.text()).toContain('manualResting')
    expect(wrapper.text()).toContain('01:00')
    expect(wrapper.get('[data-testid="account-rotation-waiting"]').text()).toContain('3')
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.text()).toContain('00:59')
    wrapper.unmount()
  })
  it.each(['manual', 'early', 'scheduled'] as const)('distinguishes %s draining from resting', async reason => {
    const wrapper = mount(AccountRotationBadge, { props: { state: { ...state, reason, phase: 'draining' } } })
    expect(wrapper.text()).toContain(`${reason}Draining`)
    expect(wrapper.find('.font-mono').exists()).toBe(false)
    expect(wrapper.get('[data-testid="account-rotation-status"]').attributes('title')).toContain('drainHoldHint')
    await wrapper.setProps({ state: { ...state, reason } })
    expect(wrapper.text()).toContain(`${reason}Resting`)
    wrapper.unmount()
  })
  it('counts down the next planned rotation and updates when the work window resets', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(now * 1000)
    const working = { ...state, phase: 'working' as const, hold: false, reason: '' as const, automatic_enabled: true, drain_at: now + 3601 }
    const wrapper = mount(AccountRotationBadge, { props: { state: working } })
    expect(wrapper.get('[data-testid="account-rotation-next"]').text()).toContain('01:00:01')
    await vi.advanceTimersByTimeAsync(1000)
    expect(wrapper.get('[data-testid="account-rotation-next"]').text()).toContain('01:00:00')
    await wrapper.setProps({ state: { ...working, drain_at: now + 61 } })
    expect(wrapper.get('[data-testid="account-rotation-next"]').text()).toContain('01:00')
    await wrapper.setProps({ state: { ...working, phase: 'ready', drain_at: 0 } })
    expect(wrapper.find('[data-testid="account-rotation-next"]').exists()).toBe(false)
    wrapper.unmount()
  })
  it('keeps both rest timestamps visible with automatic rotation off and never invents history', async () => {
    const wrapper = mount(AccountRotationBadge, { props: { state: { ...state, phase: 'ready', hold: false, waiting_count: 0, automatic_enabled: false, last_manual_rest_at: now - 3600, last_automatic_rest_at: now - 7200, last_automatic_rest_reason: 'early' } } })
    expect(wrapper.get('[data-testid="account-rotation-status"]').text()).toContain('disabled')
    expect(wrapper.get('[data-testid="account-rotation-last-manual"]').text()).not.toContain('noHistory')
    expect(wrapper.get('[data-testid="account-rotation-last-automatic"]').text()).toContain('lastEarly')
    expect(wrapper.get('[data-testid="account-rotation-last-automatic"]').attributes('title')).toContain('restStartedAt')
    expect(wrapper.find('[data-testid="account-rotation-next"]').exists()).toBe(false)
    await wrapper.setProps({ state: { ...state, last_manual_rest_at: 0, last_automatic_rest_at: 0 } })
    expect(wrapper.get('[data-testid="account-rotation-last-manual"]').text()).toContain('noHistory')
    expect(wrapper.get('[data-testid="account-rotation-last-automatic"]').text()).toContain('noHistory')
    wrapper.unmount()
  })
  it('shows the early observation window and keeps ordinary rotation separate from holding', () => {
    vi.useFakeTimers()
    vi.setSystemTime(now * 1000)
    const wrapper = mount(AccountRotationBadge, { props: { state: { ...state, phase: 'working', reason: '', hold: false, waiting_count: 0, early_eligible_at: now - 1 } } })
    expect(wrapper.text()).toContain('earlyWatching')
    expect(wrapper.find('[data-testid="account-rotation-waiting"]').exists()).toBe(false)
    wrapper.unmount()
  })
  it.each([0, 3])('shows recovery with %i queued requests, including the final gap', async waitingCount => {
    const recovering = { ...state, phase: 'working' as const, reason: '' as const, hold: false, waiting_count: waitingCount, recovering: true, retry_after_ms: 500 }
    const wrapper = mount(AccountRotationBadge, { props: { state: recovering } })
    expect(wrapper.get('[data-testid="account-rotation-status"]').text()).toContain('recovering')
    expect(wrapper.get('[data-testid="account-rotation-status"]').attributes('title')).toContain('recoveryHint')
    expect(wrapper.find('.font-mono').exists()).toBe(false)
    expect(wrapper.find('[data-testid="account-rotation-waiting"]').exists()).toBe(waitingCount > 0)
    await wrapper.setProps({ state: { ...recovering, recovering: false, retry_after_ms: 0 } })
    expect(wrapper.get('[data-testid="account-rotation-status"]').text()).toContain('working')
    wrapper.unmount()
  })
})
