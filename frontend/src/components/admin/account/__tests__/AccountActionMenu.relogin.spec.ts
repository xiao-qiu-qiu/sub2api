import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import AccountActionMenu from '../AccountActionMenu.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

enableAutoUnmount(afterEach)

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: 42,
    name: 'oauth-account',
    platform: 'openai',
    type: 'oauth',
    proxy_id: null,
    concurrency: 3,
    priority: 0,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
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

const mountMenu = (account: Account | null, reloginBusy?: boolean) => mount(AccountActionMenu, {
  props: { show: true, account, anchorRect: new DOMRect(100, 100, 24, 24), reloginBusy },
  attachTo: document.body,
  global: { stubs: { Icon: true } }
})

const reloginButton = () => document.body.querySelector<HTMLButtonElement>('[data-testid="relogin-action"]')

describe('AccountActionMenu — 自动重登', () => {
  it.each([null, undefined])('OpenAI OAuth 非影子账号显示入口（parent_account_id=%s）', (parentAccountId) => {
    mountMenu(makeAccount({ parent_account_id: parentAccountId }))

    expect(reloginButton()).not.toBeNull()
    expect(reloginButton()!.disabled).toBe(false)
  })

  it.each<Partial<Account>>([
    { parent_account_id: 7 },
    { platform: 'anthropic' },
    { platform: 'antigravity' },
    { type: 'apikey' },
    { type: 'setup-token' },
    { credentials: { auth_mode: 'agentidentity' } },
    { credentials: { auth_mode: '  AgentIdentity  ' } },
    { credentials: { auth_mode: 'personalAccessToken' } },
    { credentials: { auth_mode: 'personal_access_token' } },
    { credentials: { auth_mode: '  PERSONALACCESSTOKEN  ' } },
    { credentials: { auth_mode: '  PERSONAL_ACCESS_TOKEN  ' } },
    { credentials: { openai_auth_mode: 'personalAccessToken' } },
    { credentials: { openai_auth_mode: 'personal_access_token' } },
    { credentials: { openai_auth_mode: '  PERSONALACCESSTOKEN  ' } },
    { credentials: { openai_auth_mode: '  PERSONAL_ACCESS_TOKEN  ' } }
  ])('隐藏不支持的账号入口：%j', (overrides) => {
    mountMenu(makeAccount(overrides))

    expect(reloginButton()).toBeNull()
  })

  it('未选账号时隐藏入口', () => {
    mountMenu(null)

    expect(reloginButton()).toBeNull()
  })

  it('点击入口发送账号并请求关闭菜单', async () => {
    const account = makeAccount()
    const wrapper = mountMenu(account)
    const button = reloginButton()
    expect(button).not.toBeNull()

    button!.click()
    await wrapper.vm.$nextTick()

    expect(wrapper.emitted('relogin')).toEqual([[account]])
    expect(wrapper.emitted('close')).toEqual([[]])
  })

  it('忙时禁用按钮，点击不发送事件；恢复后可操作', async () => {
    const account = makeAccount()
    const wrapper = mountMenu(account, true)
    const button = reloginButton()
    expect(button).not.toBeNull()
    expect(button!.disabled).toBe(true)

    button!.click()
    await wrapper.vm.$nextTick()
    expect(wrapper.emitted('relogin')).toBeUndefined()
    expect(wrapper.emitted('close')).toBeUndefined()

    await wrapper.setProps({ reloginBusy: false })
    expect(reloginButton()!.disabled).toBe(false)
    reloginButton()!.click()
    await wrapper.vm.$nextTick()
    expect(wrapper.emitted('relogin')).toEqual([[account]])
    expect(wrapper.emitted('close')).toEqual([[]])
  })
})
