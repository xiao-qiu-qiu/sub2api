import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { Account } from '@/types'
import ManualAccountRestDialog from '../ManualAccountRestDialog.vue'

const { startManualRest, cancelManualRest } = vi.hoisted(() => ({ startManualRest: vi.fn(), cancelManualRest: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { startManualRest, cancelManualRest } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const state: NonNullable<Account['account_rotation']> = {
  phase: 'draining', reason: 'manual', hold: true, waiting_count: 0,
  started_at: 100, drain_at: 18000, rest_until: 0, idle_since: 0, rest_seconds: 60,
}
const account = { id: 23, name: 'test-account' } as Account
const wrappers: ReturnType<typeof mount>[] = []
function render(override: Partial<Account> = {}) {
  const wrapper = mount(ManualAccountRestDialog, {
    props: { account: { ...account, ...override } },
    global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } },
  })
  wrappers.push(wrapper)
  return wrapper
}
beforeEach(() => {
  vi.clearAllMocks()
  startManualRest.mockResolvedValue({ account_rotation: state })
  cancelManualRest.mockResolvedValue({ account_rotation: { ...state, hold: false, phase: 'working', reason: '' } })
})
afterEach(() => wrappers.splice(0).forEach(wrapper => wrapper.unmount()))

describe('ManualAccountRestDialog', () => {
  it('starts with 60 seconds and sends nothing until confirmation', async () => {
    const wrapper = render()
    expect(wrapper.get<HTMLInputElement>('#manual-rest-seconds').element.value).toBe('60')
    expect(startManualRest).not.toHaveBeenCalled()
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(startManualRest).toHaveBeenCalledWith(23, 60)
    expect(wrapper.emitted('updated')?.[0]).toEqual([state, false])
    expect(wrapper.emitted('close')).toHaveLength(1)
  })

  it('validates an edited duration and preserves it for the request', async () => {
    const wrapper = render()
    for (const seconds of ['0', '7201', '1.5', '']) {
      await wrapper.get('#manual-rest-seconds').setValue(seconds)
      await wrapper.get('form').trigger('submit')
      expect(startManualRest).not.toHaveBeenCalled()
      expect(wrapper.get<HTMLButtonElement>('button[type="submit"]').element.disabled).toBe(true)
    }
    await wrapper.get('#manual-rest-seconds').setValue('3')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(startManualRest).toHaveBeenCalledWith(23, 3)
  })

  it('allows cancel without starting a rest', async () => {
    const wrapper = render()
    await wrapper.get('button[type="button"]').trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(1)
    expect(startManualRest).not.toHaveBeenCalled()
  })

  it('ends a manual rest using DELETE, even if the list refreshes before confirmation', async () => {
    const wrapper = render({ account_rotation: state })
    expect(wrapper.find('#manual-rest-seconds').exists()).toBe(false)
    await wrapper.setProps({ account: { ...account, account_rotation: null } })
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(cancelManualRest).toHaveBeenCalledWith(23)
    expect(startManualRest).not.toHaveBeenCalled()
  })

  it('keeps the dialog open on failure and prevents duplicate confirmations while saving', async () => {
    let fail!: (error: Error) => void
    startManualRest.mockReturnValue(new Promise((_, reject) => { fail = reject }))
    const wrapper = render()
    await wrapper.get('form').trigger('submit')
    await wrapper.get('form').trigger('submit')
    expect(startManualRest).toHaveBeenCalledTimes(1)
    fail(new Error('unavailable'))
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).not.toBe('')
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.get<HTMLButtonElement>('button[type="submit"]').element.disabled).toBe(false)
  })
})
