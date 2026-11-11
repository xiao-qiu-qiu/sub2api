import { defineComponent, ref } from 'vue'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AccountCreationPreset } from '@/api/admin/accountCreationPreset'
import { useAccountCreationPreset } from '../useAccountCreationPreset'

enableAutoUnmount(afterEach)

const api = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn() }))
vi.mock('@/api/admin/accountCreationPreset', () => ({ getAccountCreationPreset: api.get, saveAccountCreationPreset: api.save }))

function preset(): AccountCreationPreset {
  return { version: 1, platform: 'openai', type: 'oauth', concurrency: 3, priority: -7, load_factor: null,
    rate_multiplier: 0, group_rate_multiplier: 0, group_ids: [12], proxy_id: null,
    auto_pause_on_expired: true, extra: { openai_excel_bps: true }, model_mapping: { client: 'upstream' } }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: Error) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function harness() {
  const context = ref('open:openai:oauth:user-1')
  const available = ref(true)
  const snapshot = ref('settings-1')
  const capture = vi.fn(() => preset())
  const apply = vi.fn()
  let state!: ReturnType<typeof useAccountCreationPreset>
  const wrapper = mount(defineComponent({
    setup() {
      state = useAccountCreationPreset({ context: () => context.value, available: () => available.value,
        snapshot: () => snapshot.value, capture, apply })
      return () => null
    }
  }))
  return { state, context, available, snapshot, capture, apply, wrapper }
}

describe('useAccountCreationPreset', () => {
  beforeEach(() => { vi.resetAllMocks(); api.get.mockResolvedValue(preset()); api.save.mockResolvedValue(preset()) })

  it('does not load or save implicitly', () => {
    const { state } = harness()
    expect(state.applied.value).toBe(false)
    expect(state.status.value).toBe('')
    expect(api.get).not.toHaveBeenCalled()
    expect(api.save).not.toHaveBeenCalled()
  })

  it('loads without capturing or validating the current form', async () => {
    const { state, capture, apply } = harness()
    capture.mockImplementation(() => { throw new Error('invalid current form') })
    await state.load()
    expect(capture).not.toHaveBeenCalled()
    expect(apply).toHaveBeenCalledTimes(1)
    expect(apply).toHaveBeenCalledWith(preset())
    expect(state.applied.value).toBe(true)
    expect(state.status.value).toBe('loaded')
    expect(state.busy.value).toBe(false)
  })

  it('explicitly saves the captured configuration with zero and null values intact', async () => {
    const { state, capture, apply } = harness()
    await state.save()
    expect(capture).toHaveBeenCalledTimes(1)
    expect(api.save).toHaveBeenCalledTimes(1)
    expect(api.save).toHaveBeenCalledWith(preset())
    expect(apply).not.toHaveBeenCalled()
    expect(state.applied.value).toBe(true)
    expect(state.status.value).toBe('saved')
  })

  it.each(['load', 'save'] as const)('does nothing when %s is unavailable', async action => {
    const { state, available, capture } = harness()
    available.value = false
    await state[action]()
    expect(api.get).not.toHaveBeenCalled()
    expect(api.save).not.toHaveBeenCalled()
    expect(capture).not.toHaveBeenCalled()
    expect(state.applied.value).toBe(false)
  })

  it('allows only one operation at a time', async () => {
    const pending = deferred<AccountCreationPreset>()
    api.get.mockReturnValue(pending.promise)
    const { state } = harness()
    const loading = state.load()
    expect(state.busy.value).toBe(true)
    await state.load()
    await state.save()
    expect(api.get).toHaveBeenCalledTimes(1)
    expect(api.save).not.toHaveBeenCalled()
    pending.resolve(preset())
    await loading
    expect(state.busy.value).toBe(false)
  })

  it.each(['load', 'save'] as const)('does not mark a failed %s as applied', async action => {
    api[action === 'load' ? 'get' : 'save'].mockRejectedValue(new Error('backend unavailable'))
    const { state, apply } = harness()
    await state[action]()
    expect(state.error.value).toBe('backend unavailable')
    expect(state.applied.value).toBe(false)
    expect(state.status.value).toBe('')
    expect(state.busy.value).toBe(false)
    expect(apply).not.toHaveBeenCalled()
  })

  it('does not mark an absent preset as applied', async () => {
    api.get.mockResolvedValue(null)
    const { state, apply } = harness()
    await state.load()
    expect(state.status.value).toBe('empty')
    expect(state.applied.value).toBe(false)
    expect(apply).not.toHaveBeenCalled()
  })

  it('reports capture validation errors without calling the backend', async () => {
    const { state, capture } = harness()
    capture.mockImplementation(() => { throw new Error('invalid BPS models') })
    await state.save()
    expect(api.save).not.toHaveBeenCalled()
    expect(state.error.value).toBe('invalid BPS models')
    expect(state.applied.value).toBe(false)
    expect(state.busy.value).toBe(false)
  })

  it('does not apply a preset rejected by the component', async () => {
    const { state, apply } = harness()
    apply.mockImplementation(() => { throw new Error('missing group') })
    await state.load()
    expect(state.error.value).toBe('missing group')
    expect(state.applied.value).toBe(false)
  })

  it.each(['close', 'platform', 'identity'] as const)('ignores a late load after changing %s', async change => {
    const pending = deferred<AccountCreationPreset>()
    api.get.mockReturnValue(pending.promise)
    const { state, context, apply } = harness()
    const loading = state.load()
    context.value = change
    expect(state.busy.value).toBe(false)
    pending.resolve(preset())
    await loading
    expect(apply).not.toHaveBeenCalled()
    expect(state.applied.value).toBe(false)
    expect(state.status.value).toBe('')
  })

  it('ignores a late load when the user has edited configuration', async () => {
    const pending = deferred<AccountCreationPreset>()
    api.get.mockReturnValue(pending.promise)
    const { state, snapshot, apply } = harness()
    const loading = state.load()
    snapshot.value = 'edited-settings'
    pending.resolve(preset())
    await loading
    expect(apply).not.toHaveBeenCalled()
    expect(state.applied.value).toBe(false)
    expect(state.status.value).toBe('changed')
  })

  it('ignores a late save after identity changes', async () => {
    const pending = deferred<AccountCreationPreset>()
    api.save.mockReturnValue(pending.promise)
    const { state, context } = harness()
    const saving = state.save()
    context.value = 'open:openai:oauth:user-2'
    pending.resolve(preset())
    await saving
    expect(state.applied.value).toBe(false)
    expect(state.status.value).toBe('')
  })

  it('ignores a result after availability is lost', async () => {
    const pending = deferred<AccountCreationPreset>()
    api.get.mockReturnValue(pending.promise)
    const { state, available, apply } = harness()
    const loading = state.load()
    available.value = false
    pending.resolve(preset())
    await loading
    expect(apply).not.toHaveBeenCalled()
    expect(state.applied.value).toBe(false)
    expect(state.busy.value).toBe(false)
  })

  it('ignores obsolete errors and does not clear a newer request busy state', async () => {
    const old = deferred<AccountCreationPreset>()
    const current = deferred<AccountCreationPreset>()
    api.get.mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise)
    const { state, context, apply } = harness()
    const first = state.load()
    context.value = 'another-context'
    const second = state.load()
    old.reject(new Error('obsolete error'))
    await first
    expect(state.error.value).toBe('')
    expect(state.busy.value).toBe(true)
    current.resolve(preset())
    await second
    expect(apply).toHaveBeenCalledTimes(1)
    expect(state.status.value).toBe('loaded')
    expect(state.busy.value).toBe(false)
  })

  it('invalidates pending responses on unmount and clears an applied preset on reset', async () => {
    const first = harness()
    await first.state.load()
    first.state.reset()
    expect(first.state.applied.value).toBe(false)
    expect(first.state.status.value).toBe('')
    const pending = deferred<AccountCreationPreset>()
    api.get.mockReturnValue(pending.promise)
    const second = harness()
    const loading = second.state.load()
    second.wrapper.unmount()
    pending.resolve(preset())
    await loading
    expect(second.apply).not.toHaveBeenCalled()
    expect(second.state.applied.value).toBe(false)
  })
})
