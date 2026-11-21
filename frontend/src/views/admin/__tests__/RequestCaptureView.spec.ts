import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import View from '../RequestCaptureView.vue'
const mocks = vi.hoisted(() => ({ listTasks: vi.fn(), createTask: vi.fn(), listRecords: vi.fn(), getRecord: vi.fn(), getContent: vi.fn(), list: vi.fn(), replace: vi.fn() }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/components/admin/operations/SmartOpsNav.vue', () => ({ default: { template: '<nav />' } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ replace: mocks.replace }) }))
vi.mock('@/stores/adminSettings', () => ({ useAdminSettingsStore: () => ({ setRequestCaptureEnabledLocal: vi.fn() }) }))
vi.mock('@/api', () => ({ adminAPI: { users: { list: mocks.list }, accounts: { list: mocks.list }, groups: { list: mocks.list } } }))
vi.mock('@/api/admin/requestCaptures', () => ({ ...mocks, canStreamExport: () => false, stopTask: vi.fn(), deleteTask: vi.fn(), exportCapture: vi.fn() }))
const task = { id: 'task', target_type: 'user', target_id: 7, target_name: 'fixture', status: 'running', requests: 1, partial: 0, skipped: 0, bytes: 100, expires_at: '2099-01-01' }
const record = { id: 'record', task_id: 'task', request_id: 'request-id', parts: [{ name: '1-client_request.txt', stage: 'client_request', bytes: 10, attempt: 0, turn: 0 }] }
const options = { global: { stubs: { AppLayout: { template: '<div><slot /></div>' } } } }
beforeEach(() => { vi.clearAllMocks(); mocks.listTasks.mockResolvedValue({ items: [task], stats: {}, has_more: false }); mocks.list.mockResolvedValue({ items: [{ id: 7, email: 'user@test' }], total: 1 }); mocks.createTask.mockResolvedValue(task); mocks.listRecords.mockResolvedValue({ items: [record], has_more: false }); mocks.getRecord.mockResolvedValue(record); mocks.getContent.mockResolvedValue({ text: '<img src=x onerror=alert(1)>', next_offset: 10, has_more: true }) })
afterEach(() => vi.restoreAllMocks())
describe('request capture page', () => {
  it('validates duration and defaults media capture off', async () => {
    const wrapper = mount(View, options); await flushPromises()
    await wrapper.findAll('select')[1]!.setValue('7')
    await wrapper.find('input[type=number]').setValue('0'); await wrapper.find('form').trigger('submit'); expect(mocks.createTask).not.toHaveBeenCalled()
    await wrapper.find('input[type=number]').setValue('1440'); await wrapper.find('form').trigger('submit'); await flushPromises()
    expect(mocks.createTask).toHaveBeenCalledWith({ target_type: 'user', target_id: 7, duration_minutes: 1440, save_media: false }); wrapper.unmount()
  })
  it('renders capture content as text and replaces preview pages', async () => {
    const wrapper = mount(View, options); await flushPromises()
    await wrapper.findAll('button').find(b => b.text().includes('#7'))!.trigger('click'); await flushPromises()
    expect(mocks.listRecords).toHaveBeenLastCalledWith('task', 1, '', false)
    expect(wrapper.findAll('input[type=checkbox]')).toHaveLength(2)
    await wrapper.findAll('button').find(b => b.text() === 'request-id')!.trigger('click'); await flushPromises()
    expect(wrapper.find('img').exists()).toBe(false); expect(wrapper.text()).toContain('<img src=x onerror=alert(1)>')
    mocks.getContent.mockResolvedValueOnce({ text: 'next page', next_offset: 19, has_more: false })
    await wrapper.findAll('button').find(b => b.text() === 'admin.requestCapture.nextSegment')!.trigger('click'); await flushPromises()
    expect(mocks.getContent).toHaveBeenLastCalledWith('task', 'record', '1-client_request.txt', 10); expect(wrapper.text()).not.toContain('<img src=x'); expect(wrapper.text()).toContain('next page'); wrapper.unmount()
  })
  it.each([
    ['failed', 'basispoints_stream_incomplete', 'streamFailed'],
    ['completed', 'upstream_read_failed', 'completedWithErrors'],
    [undefined, 'unknown_error', 'containsErrors'],
  ])('shows business outcome for HTTP 200: %s', async (outcome, code, label) => {
    mocks.listRecords.mockResolvedValue({ items: [{ ...record, status: 200, is_error: true, client_outcome: outcome, error_code: code }], has_more: false })
    const wrapper = mount(View, options); await flushPromises()
    await wrapper.findAll('button').find(b => b.text().includes('#7'))!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('HTTP 200')
    expect(wrapper.get('[data-testid="capture-outcome"]').text()).toBe('admin.requestCapture.' + label)
    expect(wrapper.text()).toContain(code)
    wrapper.unmount()
  })

  it('shows successful suspected distillation separately and can filter errors only', async () => {
    const captured = { ...record, status: 200, is_error: false, is_suspected_distillation: true, distillation_evidence: { output_tokens: 3000, cache_read_tokens: 0, client_stream: false } }
    mocks.listRecords.mockResolvedValue({ items: [captured], has_more: false })
    mocks.getRecord.mockResolvedValue(captured)
    const wrapper = mount(View, options); await flushPromises()
    await wrapper.findAll('button').find(b => b.text().includes('#7'))!.trigger('click'); await flushPromises()
    expect(mocks.listRecords).toHaveBeenLastCalledWith('task', 1, '', false)
    expect(wrapper.get('[data-testid="distillation-badge"]').text()).toBe('admin.requestCapture.suspectedDistillation')
    expect(wrapper.find('[data-testid="capture-outcome"]').exists()).toBe(false)
    await wrapper.findAll('button').find(b => b.text() === 'request-id')!.trigger('click'); await flushPromises()
    expect(wrapper.get('[data-testid="distillation-detail"]').text()).toContain('admin.requestCapture.nonStreaming')
    await wrapper.findAll('input[type=checkbox]')[1]!.setValue(true); await flushPromises()
    expect(mocks.listRecords).toHaveBeenLastCalledWith('task', 1, '', true)
    wrapper.unmount()
  })

  it('keeps the latest error filter when an older request resolves later', async () => {
    let resolveOlder!: (value: unknown) => void
    mocks.listRecords.mockReturnValueOnce(new Promise(resolve => { resolveOlder = resolve }))
    const wrapper = mount(View, options); await flushPromises()
    await wrapper.findAll('button').find(b => b.text().includes('#7'))!.trigger('click'); await flushPromises()
    mocks.listRecords.mockResolvedValueOnce({ items: [{ ...record, request_id: 'only-error', is_error: true }], has_more: false })
    await wrapper.findAll('input[type=checkbox]')[1]!.setValue(true); await flushPromises()
    expect(wrapper.text()).toContain('only-error')
    resolveOlder({ items: [{ ...record, request_id: 'old-result', is_suspected_distillation: true }], has_more: false })
    await flushPromises()
    expect(wrapper.text()).toContain('only-error')
    expect(wrapper.text()).not.toContain('old-result')
    wrapper.unmount()
  })

})
