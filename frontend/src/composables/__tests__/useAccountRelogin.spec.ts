import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount, type VueWrapper } from '@vue/test-utils'
import type { Account } from '@/types'
import type { TokenGuardV2Task } from '@/api/admin/accountTokenGuardV2'
import { useAccountRelogin } from '@/composables/useAccountRelogin'

const mocks = vi.hoisted(() => ({
  relogin: vi.fn(),
  status: vi.fn(),
  showInfo: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
  t: vi.fn((key: string, params?: Record<string, unknown>) =>
    `translated:${key}${params ? ` ${JSON.stringify(params)}` : ''}`),
}))

vi.mock('@/api/admin/accountTokenGuardV2', () => ({
  reloginTokenGuardV2Account: mocks.relogin,
  getAccountReloginStatus: mocks.status,
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: mocks.t }) }))

type ReloginAccount = Pick<Account, 'id' | 'name' | 'platform' | 'type' | 'parent_account_id' | 'credentials'>
type ReloginStatus = { task?: TokenGuardV2Task }

const account = (id = 42): ReloginAccount => ({
  id, name: `account-${id}`, platform: 'openai', type: 'oauth',
  parent_account_id: null, credentials: {},
})
const task = (overrides: Partial<TokenGuardV2Task> = {}): TokenGuardV2Task => ({
  id: 71, account_id: 42, status: 'queued', stage: 'queued', attempt: 0,
  created_at: '2026-11-08T00:00:00Z', updated_at: '2026-11-08T00:00:00Z',
  ...overrides,
})
const deferred = <T>() => {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}

describe('useAccountRelogin', () => {
  const wrappers: VueWrapper[] = []
  const setup = (onCompleted = vi.fn()) => {
    let actions!: ReturnType<typeof useAccountRelogin>
    const wrapper = mount(defineComponent({
      setup() {
        actions = useAccountRelogin(onCompleted)
        return () => null
      },
    }))
    wrappers.push(wrapper)
    return { ...actions, onCompleted, wrapper }
  }
  const settle = () => vi.advanceTimersByTimeAsync(0)
  const errors = () => mocks.showError.mock.calls.map(call => String(call[0])).join('\n')

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    mocks.relogin.mockReset().mockResolvedValue(task())
    mocks.status.mockReset().mockResolvedValue({ task: task({ status: 'running', stage: 'starting' }) })
  })

  afterEach(() => {
    wrappers.splice(0).forEach(wrapper => wrapper.unmount())
    vi.clearAllTimers()
    vi.useRealTimers()
  })

  it('reports queue acceptance as info, polls every 2s, and only succeeds after the matching task finishes', async () => {
    mocks.status
      .mockResolvedValueOnce({ task: task({ status: 'callback_processing', stage: 'applying_credentials' }) })
      .mockResolvedValueOnce({ task: task({ status: 'succeeded', stage: 'succeeded' }) })
    const actions = setup()
    void actions.relogin(account())
    expect(actions.isRelogging(42)).toBe(true)
    await settle()
    expect(mocks.showInfo).toHaveBeenCalledWith(expect.stringContaining('admin.accounts.reloginStarted'))
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(mocks.status).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1999)
    expect(mocks.status).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(mocks.status).toHaveBeenCalledTimes(1)
    expect(mocks.status.mock.calls[0]?.[0]).toBe(42)
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(actions.onCompleted).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(2000)
    expect(mocks.showSuccess).toHaveBeenCalledWith(expect.stringContaining('admin.accounts.reloginSuccess'))
    expect(actions.onCompleted).toHaveBeenCalledTimes(1)
    expect(actions.isRelogging(42)).toBe(false)
    expect(mocks.showError).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(6000)
    expect(mocks.status).toHaveBeenCalledTimes(2)
  })

  it('translates a real terminal failure using its safe structured reason', async () => {
    mocks.status.mockResolvedValue({ task: task({
      status: 'failed', stage: 'failed', error: 'private provider response',
      failure: { category: 'password_rejected', code: 'password_rejected', stage: 'web_login', action: 'update_password' },
    }) })
    const actions = setup()
    void actions.relogin(account())
    await vi.advanceTimersByTimeAsync(2000)
    expect(errors()).toContain('admin.accounts.reloginFailed')
    expect(errors()).toContain('tokenGuardV2.operationFailure.reasons.password_rejected')
    expect(errors()).not.toContain('private provider response')
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(actions.onCompleted).toHaveBeenCalledTimes(1)
    expect(actions.isRelogging(42)).toBe(false)
  })

  it('shows a localized request error when queue submission fails', async () => {
    mocks.relogin.mockRejectedValue({
      status: 400, reason: 'OPENAI_REAUTH_CONFIG_REQUIRED', message: 'save login configuration first',
    })
    const actions = setup()
    void actions.relogin(account())
    await settle()
    expect(mocks.showError).toHaveBeenCalledTimes(1)
    expect(errors()).toContain('admin.accounts.reloginConfigRequired')
    expect(errors()).not.toContain('save login configuration first')
    expect(mocks.showInfo).not.toHaveBeenCalled()
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(mocks.status).not.toHaveBeenCalled()
    expect(actions.isRelogging(42)).toBe(false)
  })

  it('reports an unknown result after three query errors, a missing task, or a different task ID', async () => {
    const scenarios = [
      { polls: 3, query: () => Promise.reject(new Error('temporary connection failure')) },
      { polls: 1, query: () => Promise.resolve({}) },
      { polls: 1, query: () => Promise.resolve({ task: task({ id: 72, status: 'succeeded', stage: 'succeeded' }) }) },
    ]
    for (const scenario of scenarios) {
      mocks.status.mockReset().mockImplementation(scenario.query)
      mocks.showError.mockClear()
      const actions = setup()
      void actions.relogin(account())
      await settle()
      for (let poll = 1; poll < scenario.polls; poll++) {
        await vi.advanceTimersByTimeAsync(2000)
        expect(mocks.showError).not.toHaveBeenCalled()
        expect(actions.isRelogging(42)).toBe(true)
      }
      await vi.advanceTimersByTimeAsync(2000)
      expect(mocks.status).toHaveBeenCalledTimes(scenario.polls)
      expect(errors()).toContain('admin.accounts.reloginResultUnavailable')
      expect(errors()).not.toContain('admin.accounts.reloginFailed')
      expect(mocks.showSuccess).not.toHaveBeenCalled()
      expect(actions.isRelogging(42)).toBe(false)
      await vi.advanceTimersByTimeAsync(6000)
      expect(mocks.status).toHaveBeenCalledTimes(scenario.polls)
    }
  })

  it('ends a task still running after 15 minutes with an unknown-result message', async () => {
    const actions = setup()
    void actions.relogin(account())
    await vi.advanceTimersByTimeAsync(15 * 60_000 - 1)
    expect(actions.isRelogging(42)).toBe(true)
    expect(mocks.showError).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(errors()).toContain('admin.accounts.reloginResultUnavailable')
    expect(errors()).not.toContain('admin.accounts.reloginFailed')
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(actions.isRelogging(42)).toBe(false)
    const calls = mocks.status.mock.calls.length
    await vi.advanceTimersByTimeAsync(4000)
    expect(mocks.status).toHaveBeenCalledTimes(calls)
  })

  it('deduplicates the same account while letting different accounts complete independently', async () => {
    mocks.relogin.mockImplementation(async (id: number) => task({ id: id + 100, account_id: id }))
    mocks.status.mockImplementation(async (id: number) => ({ task: task({
      id: id + 100, account_id: id,
      status: id === 42 ? 'succeeded' : 'running', stage: id === 42 ? 'succeeded' : 'starting',
    }) }))
    const actions = setup()
    void actions.relogin(account(42))
    void actions.relogin(account(42))
    void actions.relogin(account(43))
    await settle()
    expect(mocks.relogin.mock.calls.map(call => call[0])).toEqual([42, 43])
    expect(actions.isRelogging(42)).toBe(true)
    expect(actions.isRelogging(43)).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(actions.isRelogging(42)).toBe(false)
    expect(actions.isRelogging(43)).toBe(true)
    expect(actions.onCompleted).toHaveBeenCalledTimes(1)
    expect(mocks.showSuccess).toHaveBeenCalledTimes(1)
    mocks.status.mockResolvedValue({ task: task({ id: 143, account_id: 43, status: 'succeeded', stage: 'succeeded' }) })
    await vi.advanceTimersByTimeAsync(2000)
    expect(actions.isRelogging(43)).toBe(false)
    expect(actions.onCompleted).toHaveBeenCalledTimes(2)
    expect(mocks.showSuccess).toHaveBeenCalledTimes(2)
  })

  it('clears a queued polling timer on unmount', async () => {
    const actions = setup()
    void actions.relogin(account())
    await settle()
    const signal = mocks.relogin.mock.calls[0]?.[1] as AbortSignal | undefined
    expect(signal).toBeDefined()
    actions.wrapper.unmount()
    expect(signal?.aborted).toBe(true)
    expect(vi.getTimerCount()).toBe(0)
    const infos = mocks.showInfo.mock.calls.length
    await vi.advanceTimersByTimeAsync(15 * 60_000)
    expect(mocks.status).not.toHaveBeenCalled()
    expect(mocks.showInfo).toHaveBeenCalledTimes(infos)
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(mocks.showError).not.toHaveBeenCalled()
    expect(actions.onCompleted).not.toHaveBeenCalled()
  })

  it('aborts in-flight submission and polling on unmount and ignores their late responses', async () => {
    const submission = deferred<TokenGuardV2Task>()
    const query = deferred<ReloginStatus>()
    mocks.relogin.mockImplementation((id: number) => id === 43 ? submission.promise : Promise.resolve(task()))
    mocks.status.mockReturnValue(query.promise)
    const actions = setup()
    void actions.relogin(account(42))
    void actions.relogin(account(43))
    await vi.advanceTimersByTimeAsync(2000)
    const submitSignal = mocks.relogin.mock.calls.find(call => call[0] === 43)?.[1] as AbortSignal | undefined
    const querySignal = mocks.status.mock.calls[0]?.[1] as AbortSignal | undefined
    expect(submitSignal).toBeDefined()
    expect(querySignal).toBeDefined()
    actions.wrapper.unmount()
    expect(submitSignal?.aborted).toBe(true)
    expect(querySignal?.aborted).toBe(true)
    expect(vi.getTimerCount()).toBe(0)
    const infos = mocks.showInfo.mock.calls.length
    submission.resolve(task({ id: 72, account_id: 43 }))
    query.resolve({ task: task({ status: 'succeeded', stage: 'succeeded' }) })
    await settle()
    await vi.advanceTimersByTimeAsync(6000)
    expect(mocks.showInfo).toHaveBeenCalledTimes(infos)
    expect(mocks.showSuccess).not.toHaveBeenCalled()
    expect(mocks.showError).not.toHaveBeenCalled()
    expect(actions.onCompleted).not.toHaveBeenCalled()
    expect(mocks.status).toHaveBeenCalledTimes(1)
    expect(vi.getTimerCount()).toBe(0)
  })
})
