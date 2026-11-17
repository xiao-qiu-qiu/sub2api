import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import Dialog from '../AccountOpsWebhookDialog.vue'
import { saveAccountOpsWebhook } from '@/api/admin/accountOps'
vi.mock('@/api/admin/accountOps', () => ({ saveAccountOpsWebhook: vi.fn() }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
beforeEach(() => vi.clearAllMocks())
describe('independent bot editor', () => {
  it('saves one bot and erases transient credentials when closing', async () => {
    vi.mocked(saveAccountOpsWebhook).mockResolvedValue({ enabled: false, recipient: '', balance_low: true, weekly_quota: true, cooldown_minutes: 60 })
    const props = { show: true, hook: { id: 'one', provider: 'feishu' as const, enabled: true, url_configured: true }, encryptionConfigured: true }
    const wrapper = mount(Dialog, { props, global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } } })
    await wrapper.get('[data-testid="account-ops-webhook-name-one"]').setValue('值班群')
    await wrapper.get('[data-testid="account-ops-webhook-secret-one"]').setValue('test-only')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(saveAccountOpsWebhook).toHaveBeenCalledWith('one', { name: '值班群', provider: 'auto', enabled: true, secret: 'test-only' })
    expect((wrapper.get('[data-testid="account-ops-webhook-secret-one"]').element as HTMLInputElement).value).toBe('')
    expect(wrapper.emitted('saved')).toHaveLength(1)
    wrapper.unmount()
  })

  it('saves only the PushPlus token and clears it before reopening the editor', async () => {
    vi.mocked(saveAccountOpsWebhook).mockResolvedValue({ enabled: false, recipient: '', balance_low: true, weekly_quota: true, cooldown_minutes: 60 })
    const wrapper = mount(Dialog, { props: { show: true, hook: null, provider: 'pushplus', encryptionConfigured: true }, global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } } })
    const token = wrapper.get('[data-testid^="account-ops-webhook-token-"]')
    await token.setValue('test-only-token')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(saveAccountOpsWebhook).toHaveBeenCalledWith(expect.any(String), { name: '', provider: 'pushplus', enabled: true, token: 'test-only-token' })
    expect((token.element as HTMLInputElement).value).toBe('')
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    expect((wrapper.get('[data-testid^="account-ops-webhook-token-"]').element as HTMLInputElement).value).toBe('')
    wrapper.unmount()
  })
})
