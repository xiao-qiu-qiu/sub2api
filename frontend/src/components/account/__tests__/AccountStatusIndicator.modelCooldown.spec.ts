import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, shallowMount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import AccountStatusIndicator from '../AccountStatusIndicator.vue'
import enAccounts from '@/i18n/locales/en/admin/accounts'
import zhAccounts from '@/i18n/locales/zh/admin/accounts'
import enQualityOps from '@/i18n/locales/en/qualityOps'
import zhQualityOps from '@/i18n/locales/zh/qualityOps'
import type { Account } from '@/types'

vi.mock('@/utils/format', async () => ({
  ...await vi.importActual<typeof import('@/utils/format')>('@/utils/format'),
  formatCountdown: () => '30m',
  formatDateTime: () => '2026-11-11 12:30',
  formatDateTimeToMinute: () => '2026-11-11 12:30'
}))

const now = '2026-11-11T12:00:00Z'
const resetAt = '2026-11-11T12:30:00Z'
const model = 'gpt-6.1-sol'
const unavailableCases = [
  { reason: 'upstream_400_codex_plan_gated_model', permission: true },
  { reason: 'upstream_404_model_not_found', permission: false },
  { reason: 'upstream_401_model_not_found', permission: false },
  { reason: 'openai_image_capability_lost', permission: false }
]

function modelLimit(reason?: string, until = resetAt) {
  return { reason, rate_limited_at: now, rate_limit_reset_at: until }
}

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'openai',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: now,
    updated_at: now,
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides
  }
}

function mountStatus(locale: 'zh' | 'en', account: Account) {
  return shallowMount(AccountStatusIndicator, {
    props: { account },
    global: {
      plugins: [createI18n({
        legacy: false,
        locale,
        messages: {
          zh: { admin: zhAccounts, qualityOps: zhQualityOps },
          en: { admin: enAccounts, qualityOps: enQualityOps }
        }
      })],
      stubs: {
        HelpTooltip: {
          template: '<div data-testid="status-tooltip"><div data-testid="status-trigger"><slot name="trigger" /></div><div data-testid="status-detail"><slot /></div></div>'
        }
      }
    }
  })
}

enableAutoUnmount(afterEach)
beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date(now))
})
afterEach(() => vi.useRealTimers())

describe.each([
  { locale: 'zh' as const, unavailable: '模型暂不可用', permission: '权限冷却', rateLimit: '限流至', credits: '正在使用 AI Credits', exhausted: '积分已用尽', quality: '等待质量恢复', rateLimited: '限流中' },
  { locale: 'en' as const, unavailable: 'Model unavailable', permission: 'Permission cooldown', rateLimit: 'rate limited until', credits: 'using AI Credits', exhausted: 'Credits Exhausted', quality: 'Awaiting quality recovery', rateLimited: 'Rate Limited' }
])('model cooldown status ($locale)', labels => {
  const { locale } = labels

  it.each(unavailableCases.flatMap(testCase => [false, true].map(allowOverages => ({ ...testCase, allowOverages }))))(
    'shows the availability cause for $reason with overages=$allowOverages',
    ({ reason, permission, allowOverages }) => {
      const wrapper = mountStatus(locale, makeAccount({
        extra: { allow_overages: allowOverages, model_rate_limits: { [model]: modelLimit(reason) } }
      }))
      const trigger = wrapper.get('[data-testid="status-trigger"]')
      expect(trigger.text()).toContain(model)
      expect(trigger.text()).toContain(permission ? labels.permission : labels.unavailable)
      expect(trigger.text()).toContain('30m')
      expect(trigger.get('span').classes()).toContain('bg-purple-100')
      const detail = wrapper.get('[data-testid="status-detail"]').text()
      expect(detail).toContain(model)
      expect(detail).toContain('2026-11-11 12:30')
      expect(detail).toContain(locale === 'zh' ? '到期后可重试' : 'retry after cooldown')
      expect(detail).not.toContain(labels.rateLimit)
      expect(wrapper.text()).not.toContain('⚡')
      expect(wrapper.text()).not.toContain(labels.credits)
      expect(wrapper.text()).not.toContain('admin.accounts.status.')
      expect(wrapper.text()).not.toContain('429')
    }
  )

  it.each([undefined, 'upstream_429', 'openai_image_rate_limited', 'openai_codex_spark_rate_limit', 'upstream_400_invalid_request', 'toString'])(
    'preserves ordinary rate-limit behavior for reason=%s', reason => {
      const wrapper = mountStatus(locale, makeAccount({ extra: { model_rate_limits: { [model]: modelLimit(reason) } } }))
      expect(wrapper.get('[data-testid="status-detail"]').text()).toContain(labels.rateLimit)
      expect(wrapper.text()).not.toContain(labels.unavailable)
      expect(wrapper.text()).not.toContain(labels.permission)
    }
  )

  it('keeps credit overages available for real model rate limits', () => {
    const wrapper = mountStatus(locale, makeAccount({
      platform: 'antigravity',
      extra: { allow_overages: true, model_rate_limits: { [model]: modelLimit('upstream_429') } }
    }))
    expect(wrapper.text()).toContain('⚡')
    expect(wrapper.get('[data-testid="status-detail"]').text()).toContain(labels.credits)
  })

  it('keeps exhausted credits, true rate limits and permission cooldowns distinct', () => {
    const wrapper = mountStatus(locale, makeAccount({
      extra: {
        allow_overages: true,
        model_rate_limits: {
          AICredits: modelLimit(),
          [model]: modelLimit('upstream_400_codex_plan_gated_model'),
          'rate-limited-model': modelLimit('upstream_429')
        }
      }
    }))
    expect(wrapper.findAll('[data-testid="status-tooltip"]')).toHaveLength(3)
    expect(wrapper.text()).toContain(labels.exhausted)
    expect(wrapper.text()).toContain(labels.permission)
    expect(wrapper.text()).toContain(labels.rateLimit)
    expect(wrapper.text()).not.toContain('⚡')
  })

  it.each(['2026-11-11T11:59:59Z', now])('hides all expired availability cooldowns at %s', until => {
    const wrapper = mountStatus(locale, makeAccount({
      extra: { model_rate_limits: Object.fromEntries(unavailableCases.map(({ reason }) => [reason, modelLimit(reason, until)])) }
    }))
    expect(wrapper.find('[data-testid="status-tooltip"]').exists()).toBe(false)
  })

  it('keeps an expired quality hold waiting for quality recovery with overages enabled', () => {
    const wrapper = mountStatus(locale, makeAccount({
      extra: { allow_overages: true, model_rate_limits: { [model]: modelLimit('quality_rule:7', '2020-01-01T00:00:00Z') } }
    }))
    expect(wrapper.get('[data-testid="status-trigger"]').text()).toContain(labels.quality)
    expect(wrapper.get('[data-testid="status-detail"]').text()).toContain(locale === 'zh' ? '质量冷却' : 'Quality cooldown')
    expect(wrapper.text()).not.toContain('⚡')
    expect(wrapper.text()).not.toContain(labels.permission)
    expect(wrapper.text()).not.toContain(labels.rateLimit)
  })

  it('updates the cause when refreshed and removes the badge after cooldown', async () => {
    const account = makeAccount({
      extra: { model_rate_limits: { [model]: modelLimit('upstream_400_codex_plan_gated_model') } }
    })
    const wrapper = mountStatus(locale, account)
    expect(wrapper.text()).toContain(labels.permission)
    await wrapper.setProps({ account: { ...account, extra: { model_rate_limits: { [model]: modelLimit('upstream_429') } } } })
    expect(wrapper.text()).not.toContain(labels.permission)
    expect(wrapper.text()).toContain(labels.rateLimit)
    await wrapper.setProps({ account: { ...account, extra: { model_rate_limits: {} } } })
    expect(wrapper.find('[data-testid="status-tooltip"]').exists()).toBe(false)
  })

  it('preserves a real account-wide 429 alongside a model permission cooldown', () => {
    const wrapper = mountStatus(locale, makeAccount({
      rate_limit_reset_at: resetAt,
      extra: { model_rate_limits: { [model]: modelLimit('upstream_400_codex_plan_gated_model') } }
    }))
    expect(wrapper.get('.badge-warning').text()).toBe(labels.rateLimited)
    expect(wrapper.text()).toContain('429')
    expect(wrapper.text()).toContain(labels.permission)
    expect(wrapper.findAll('[data-testid="status-tooltip"]')).toHaveLength(2)
  })
})
