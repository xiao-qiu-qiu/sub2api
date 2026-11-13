import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DOMWrapper, flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import AccountsView from '../AccountsView.vue'
import AccountCapacityCell from '@/components/account/AccountCapacityCell.vue'
import CapacityBadge from '@/components/account/CapacityBadge.vue'
import UpstreamBillingRateCell from '@/components/account/UpstreamBillingRateCell.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'

const {
  listAccounts,
  listWithEtag,
  getById,
  getPerformance,
  getBatchTodayStats,
  getManagementCapabilities,
  getUpstreamBillingProbeSettings,
  getUpstreamBillingRatesWithEtag,
  probeUpstreamBilling,
  getAllProxies,
  getAllGroups,
  refreshCredentials,
  translate,
  showError,
  showWarning
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getPerformance: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getManagementCapabilities: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getUpstreamBillingRatesWithEtag: vi.fn(),
  probeUpstreamBilling: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  refreshCredentials: vi.fn(),
  translate: vi.fn((key: string, _values?: Record<string, unknown>) => key),
  showError: vi.fn(),
  showWarning: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getManagementCapabilities,
      list: listAccounts,
      getById,
      getPerformance,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      getUpstreamBillingRatesWithEtag,
      probeUpstreamBilling,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
      refreshCredentials
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showWarning, showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: translate }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] }, columns: { type: Array, default: () => [] } },
  emits: ['sort'],
  template: `
    <div>
      <div v-for="row in data" :key="row.id" :data-account-name="row.name">
        <slot name="cell-groups" :row="row" />
        <slot name="cell-capacity" :row="row" />
        <slot name="cell-performance" :row="row" />
        <slot name="cell-upstream_billing_rate" :row="row" />
        <slot name="cell-actions" :row="row" />
      </div>
    </div>
  `
})

const AccountGroupsCellStub = defineComponent({
  props: { groups: { type: Array, default: () => [] } },
  template: '<span data-test="account-groups">{{ groups.map(group => group.name).join(",") }}</span>'
})

const EditAccountModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="edit-account">{{ show ? account?.name : "" }}</div>'
})

const AccountTestModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="test-account">{{ show ? account?.name : "" }}</div>'
})

const AccountStatsModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="stats-account">{{ show ? account?.name : "" }}</div>'
})

function mountView(stubActionMenu = true) {
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: stubActionMenu,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: AccountTestModalStub,
        AccountStatsModal: AccountStatsModalStub,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: EditAccountModalStub,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: false,
        AccountPerformanceCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: AccountGroupsCellStub,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: stubActionMenu
      }
    }
  })
}

const listRow = {
  id: 42,
  name: 'compact row',
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  group_ids: [7],
  extra: { auto_config_concurrency: { concurrency: 2, successes: 1, required: 20, maximum: 100, step: 1 } },
  credentials: {}
}

const fullAccount = {
  ...listRow,
  groups: [{ id: 7, name: 'codex', platform: 'openai' }],
  account_groups: [{ account_id: 42, group_id: 7 }],
  credentials: { api_key: 'redacted' },
  extra: { detail_only: true }
}

describe('admin AccountsView lite account list', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({ items: [listRow], total: 1, page: 1, page_size: 20, pages: 1 })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'compact-etag', data: null })
    getById.mockReset().mockResolvedValue(fullAccount)
    getPerformance.mockReset().mockResolvedValue({})
    getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
    getManagementCapabilities.mockReset().mockResolvedValue({ concurrency_upgrade_enabled: false })
    getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
    getAllProxies.mockReset().mockResolvedValue([])
    getAllGroups.mockReset().mockResolvedValue([{ id: 7, name: 'codex', platform: 'openai' }])
    getUpstreamBillingRatesWithEtag.mockReset().mockResolvedValue({ notModified: true, data: null })
    probeUpstreamBilling.mockReset()
    refreshCredentials.mockReset()
    translate.mockReset().mockImplementation(key => key)
    showError.mockReset()
    showWarning.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('keeps the refresh menu in a narrow viewport and repositions it on scroll and resize', async () => {
    const viewportWidth = vi.spyOn(window, 'innerWidth', 'get').mockReturnValue(200)
    const viewportHeight = vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(400)
    vi.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(0)
    const wrapper = mountView()
    await flushPromises()
    const trigger = wrapper.get('[data-testid="account-auto-refresh"] > button')
    const bounds = vi.spyOn(trigger.element, 'getBoundingClientRect')
      .mockReturnValue({ top: 40, bottom: 80, right: 60 } as DOMRect)
    await trigger.trigger('click')
    const menu = wrapper.get<HTMLElement>('[data-testid="auto-refresh-menu"]')
    expect(menu.element.style).toMatchObject({ top: '88px', bottom: 'auto', left: '16px', width: '168px', maxHeight: '280px' })
    expect(menu.findAll('button')).toHaveLength(5)

    viewportWidth.mockReturnValue(393)
    viewportHeight.mockReturnValue(844)
    window.dispatchEvent(new Event('resize'))
    await flushPromises()
    expect(menu.element.style).toMatchObject({ top: '88px', left: '16px', width: '224px', maxHeight: '590px' })

    bounds.mockReturnValue({ top: 760, bottom: 800, right: 60 } as DOMRect)
    window.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(menu.element.style).toMatchObject({ top: 'auto', bottom: '92px', maxHeight: '590px' })
    expect(listAccounts).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('keeps the refresh menu anchored on desktop and preserves toggling, settings and outside dismissal', async () => {
    vi.spyOn(window, 'innerWidth', 'get').mockReturnValue(1280)
    vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(900)
    // The layout viewport can be narrower than innerWidth because of the scrollbar.
    vi.spyOn(document.documentElement, 'clientWidth', 'get').mockReturnValue(1200)
    const wrapper = mountView()
    await flushPromises()
    const trigger = wrapper.get('[data-testid="account-auto-refresh"] > button')
    vi.spyOn(trigger.element, 'getBoundingClientRect')
      .mockReturnValue({ top: 40, bottom: 80, right: 1195 } as DOMRect)
    await trigger.trigger('click')
    const menu = wrapper.get<HTMLElement>('[data-testid="auto-refresh-menu"]')
    expect(menu.element.style).toMatchObject({ left: '960px', width: '224px' })
    await menu.findAll('button')[2].trigger('click')
    expect(JSON.parse(localStorage.getItem('account-auto-refresh')!)).toMatchObject({ interval_seconds: 10 })
    expect(menu.exists()).toBe(true)

    const toolsTrigger = wrapper.get('button[title="admin.accounts.moreActions"]')
    await toolsTrigger.trigger('click')
    expect(wrapper.find('[data-testid="auto-refresh-menu"]').exists()).toBe(false)
    expect(toolsTrigger.attributes('aria-expanded')).toBe('true')
    await trigger.trigger('click')
    expect(toolsTrigger.attributes('aria-expanded')).toBe('false')
    expect(wrapper.find('[data-testid="auto-refresh-menu"]').exists()).toBe(true)
    await trigger.trigger('click')
    expect(wrapper.find('[data-testid="auto-refresh-menu"]').exists()).toBe(false)

    await trigger.trigger('click')
    document.body.click()
    await flushPromises()
    expect(wrapper.find('[data-testid="auto-refresh-menu"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('refreshes the saved cost after probing even with unchanged snapshot and default sorting', async () => {
    const snapshot = { status: 'ok', data: { effective_rate_multiplier: 0.14 } }
    const row = { ...listRow, type: 'apikey', extra: { cost_multiplier: 0.1, upstream_billing_probe: snapshot } }
    listAccounts.mockResolvedValue({ items: [row], total: 1, page: 1, page_size: 20, pages: 1 })
    probeUpstreamBilling.mockResolvedValue({ account_id: row.id, snapshot })
    getUpstreamBillingRatesWithEtag.mockResolvedValue({ notModified: false, data: {
      items: [{ account_id: row.id, snapshot, cost_multiplier: 0.14 }], total: 1, page: 1, page_size: 20
    } })
    const wrapper = mountView()
    await flushPromises()
    wrapper.findComponent(UpstreamBillingRateCell).vm.$emit('probe')
    await flushPromises()
    expect(probeUpstreamBilling).toHaveBeenCalledWith(row.id)
    expect(getUpstreamBillingRatesWithEtag).toHaveBeenCalled()
    expect(wrapper.findComponent(UpstreamBillingRateCell).props('account').extra?.cost_multiplier).toBe(0.14)
    wrapper.unmount()
  })

  it('refreshes performance evidence every 30 seconds independently of unchanged list ETags', async () => {
    vi.useFakeTimers()
    const wrapper = mountView()
    await flushPromises()
    expect(getPerformance).toHaveBeenCalledWith([42], expect.any(AbortSignal))
    getPerformance.mockClear()
    await vi.advanceTimersByTimeAsync(30_000)
    await flushPromises()
    expect(getPerformance).toHaveBeenCalledTimes(1)
    expect(getPerformance).toHaveBeenCalledWith([42], expect.any(AbortSignal))
    wrapper.unmount()
  })

  it('keeps lite=1 on the initial list request', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(listAccounts).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    wrapper.unmount()
  })

  it('sorts capacity by current concurrency and resets the page while retaining filters', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'openai', status: 'active' }))
    listAccounts.mockResolvedValue({ items: [listRow], total: 60, page: 1, page_size: 20, pages: 3 })
    const wrapper = mountView()
    await flushPromises()
    const table = wrapper.getComponent(DataTableStub)
    expect(table.props('columns')).toContainEqual(expect.objectContaining({ key: 'capacity', sortable: true }))

    wrapper.getComponent({ name: 'Pagination' }).vm.$emit('update:page', 2)
    await flushPromises()
    expect(listAccounts.mock.lastCall?.[0]).toBe(2)
    table.vm.$emit('sort', 'capacity', 'desc')
    await flushPromises()
    expect(listAccounts).toHaveBeenLastCalledWith(1, 20, expect.objectContaining({
      sort_by: 'current_concurrency', sort_order: 'desc', platform: 'openai', status: 'active', lite: '1'
    }), expect.objectContaining({ signal: expect.any(AbortSignal) }))
    table.vm.$emit('sort', 'capacity', 'asc')
    await flushPromises()
    expect(listAccounts.mock.lastCall?.[2]).toMatchObject({ sort_by: 'current_concurrency', sort_order: 'asc' })
    wrapper.unmount()
  })

  it('refreshes concurrency counts together with order after a billing probe', async () => {
    localStorage.setItem('account-table-sort', JSON.stringify({ key: 'capacity', order: 'desc' }))
    const leader = { ...listRow, id: 99, current_concurrency: 9, concurrency: 100 }
    const follower = { ...listRow, current_concurrency: 2 }
    listAccounts.mockResolvedValue({ items: [leader, follower], total: 2, page: 1, page_size: 20, pages: 1 })
    const wrapper = mountView()
    await flushPromises()
    const refreshed = [{ ...follower, current_concurrency: 10 }, { ...leader, current_concurrency: 1 }]
    listAccounts.mockResolvedValue({ items: refreshed, total: 2, page: 1, page_size: 20, pages: 1 })
    getUpstreamBillingRatesWithEtag.mockResolvedValue({ notModified: false, data: {
      items: [{ account_id: 42, snapshot: null }, { account_id: 99, snapshot: null }],
      total: 2, page: 1, page_size: 20
    } })
    probeUpstreamBilling.mockResolvedValue({ account_id: 42, snapshot: { status: 'ok', data: { effective_rate_multiplier: 0.14 } } })
    wrapper.findAllComponents(UpstreamBillingRateCell)[1].vm.$emit('probe')
    await flushPromises()
    expect(listAccounts).toHaveBeenCalledTimes(2)
    expect(wrapper.getComponent(DataTableStub).props('data')).toEqual(refreshed)
    wrapper.unmount()
  })

  it('restores capacity sorting and uses server order on automatic refresh across page boundaries', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-table-sort', JSON.stringify({ key: 'capacity', order: 'desc' }))
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const refreshed = { ...listRow, id: 99, name: 'refreshed leader', current_concurrency: 9, concurrency: 100 }
    listWithEtag.mockResolvedValue({ notModified: false, etag: 'fresh-concurrency', data: {
      items: [refreshed], total: 1, page: 1, page_size: 20, pages: 1
    } })
    const wrapper = mountView()
    await flushPromises()
    expect(listAccounts.mock.lastCall?.[2]).toMatchObject({ sort_by: 'current_concurrency', sort_order: 'desc' })
    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()
    expect(listWithEtag).toHaveBeenCalledWith(1, 20, expect.objectContaining({
      sort_by: 'current_concurrency', sort_order: 'desc', lite: '1'
    }), expect.objectContaining({ etag: null }))
    expect(wrapper.getComponent(DataTableStub).props('data')).toEqual([refreshed])
    wrapper.unmount()
  })

  it('refreshes capacity congestion and queue counts even when concurrency stays unchanged', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const queueLabel = 'admin.accounts.capacity.queuedRequests'
    translate.mockImplementation((key, values) => key === queueLabel ? `${key}: ${values?.count}` : key)
    const row = { ...listRow, current_concurrency: 2, current_waiting: 0 }
    listAccounts.mockResolvedValue({ items: [row], total: 1, page: 1, page_size: 20, pages: 1 })
    for (const [index, refreshed] of [
      { ...row, current_waiting: 3 },
      { ...row, current_waiting: 0 },
      { ...row, current_concurrency: 1, current_waiting: 3 }
    ].entries()) {
      listWithEtag.mockResolvedValueOnce({ notModified: false, etag: `queue-${index}`, data: {
        items: [refreshed], total: 1, page: 1, page_size: 20, pages: 1
      } })
    }
    const wrapper = mountView()
    await flushPromises()
    const capacity = () => wrapper.getComponent(AccountCapacityCell)
    const badge = () => capacity().getComponent(CapacityBadge)
    expect(badge().classes()).toContain('bg-yellow-100')
    expect(capacity().text()).not.toContain(queueLabel)

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()
    expect(listWithEtag).toHaveBeenCalledTimes(1)
    expect(badge().classes()).toContain('bg-red-100')
    expect(capacity().text()).toContain(`${queueLabel}: 3`)

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()
    expect(listWithEtag).toHaveBeenCalledTimes(2)
    expect(badge().classes()).toContain('bg-yellow-100')
    expect(badge().classes()).not.toContain('bg-red-100')
    expect(capacity().text()).not.toContain(queueLabel)

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()
    expect(listWithEtag).toHaveBeenCalledTimes(3)
    expect(badge().classes()).toContain('bg-yellow-100')
    expect(badge().classes()).not.toContain('bg-red-100')
    expect(capacity().text()).not.toContain(queueLabel)
    wrapper.unmount()
  })

  it('ignores an older automatic refresh after capacity sort changes', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    let completeRefresh!: (value: unknown) => void
    listWithEtag.mockImplementation(() => new Promise(resolve => { completeRefresh = resolve }))
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(6000)
    const sortedRow = { ...listRow, id: 99, name: 'new sort leader' }
    listAccounts.mockResolvedValue({ items: [sortedRow], total: 1, page: 1, page_size: 20, pages: 1 })
    wrapper.getComponent(DataTableStub).vm.$emit('sort', 'capacity', 'desc')
    await flushPromises()
    completeRefresh({ notModified: false, etag: 'old-sort', data: {
      items: [listRow], total: 1, page: 1, page_size: 20, pages: 1
    } })
    await flushPromises()
    expect(wrapper.getComponent(DataTableStub).props('data')).toEqual([sortedRow])
    wrapper.unmount()
  })

  it('hides stored upgrade progress until the global switch is enabled', async () => {
    let resolveCapabilities!: (value: { concurrency_upgrade_enabled: boolean }) => void
    getManagementCapabilities.mockReturnValueOnce(new Promise(resolve => { resolveCapabilities = resolve }))
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('progress').exists()).toBe(false)

    resolveCapabilities({ concurrency_upgrade_enabled: true })
    await flushPromises()
    expect(wrapper.get('progress').attributes()).toMatchObject({ value: '1', max: '20' })
    wrapper.unmount()
  })

  it.each([false, undefined])('hides historical progress when the upgrade flag is %s', async enabled => {
    getManagementCapabilities.mockResolvedValue({ concurrency_upgrade_enabled: enabled })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('progress').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('autoConfig.tier')
    expect(wrapper.text()).not.toContain('autoConfig.progress')
    wrapper.unmount()
  })

  it('hides upgrade progress after disabling even when the account ETag is unchanged', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    getManagementCapabilities.mockResolvedValue({ concurrency_upgrade_enabled: true })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('progress').exists()).toBe(true)

    getManagementCapabilities.mockResolvedValue({ concurrency_upgrade_enabled: false })
    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()
    expect(listWithEtag).toHaveBeenCalled()
    expect(wrapper.find('progress').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('autoConfig.tier')
    wrapper.unmount()
  })

  it('hides upgrade progress if the switch cannot be read', async () => {
    getManagementCapabilities.mockRejectedValue(new Error('settings unavailable'))
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('[data-account-name]').exists()).toBe(true)
    expect(wrapper.find('progress').exists()).toBe(false)
    wrapper.unmount()
  })

  it('maps group_ids through the group catalog for the table cell', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="account-groups"]').text()).toBe('codex')
    wrapper.unmount()
  })

  it('keeps the action menu open during internal scrolling but closes it on table scrolling', async () => {
    const wrapper = mountView(false)
    await flushPromises()

    const trigger = wrapper.findAll('button').find(button => button.text() === 'common.more')!
    await trigger.trigger('click')
    const menu = new DOMWrapper(document.body.querySelector('.action-menu-content')!)
    menu.element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    menu.get('button').element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    wrapper.getComponent(DataTableStub).element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(false)
    wrapper.unmount()
  })

  it('keeps lite=1 on automatic ETag refreshes', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const wrapper = mountView()
    await flushPromises()

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()

    expect(listWithEtag).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ etag: null })
    )
    wrapper.unmount()
  })

  it('loads the full account by id before opening edit, test, and stats actions', async () => {
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    expect(editButton).toBeTruthy()
    await editButton!.trigger('click')
    await flushPromises()
    expect(getById).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-test="edit-account"]').text()).toBe('compact row')

    const menu = wrapper.findComponent(AccountActionMenu)
    menu.vm.$emit('test', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="test-account"]').text()).toBe('compact row')

    menu.vm.$emit('stats', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(3)
    expect(wrapper.get('[data-test="stats-account"]').text()).toBe('compact row')
    wrapper.unmount()
  })

  it('shows the warning and patches the account after a partial Antigravity refresh', async () => {
    refreshCredentials.mockResolvedValue({
      account: { ...fullAccount, name: 'refreshed account' },
      message: 'Token refreshed, but project_id is temporarily unavailable',
      warning: 'missing_project_id_temporary'
    })
    const wrapper = mountView(false)
    await flushPromises()

    wrapper.findComponent(AccountActionMenu).vm.$emit('refresh-token', listRow)
    await flushPromises()

    expect(refreshCredentials).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-account-name]').attributes('data-account-name')).toBe('refreshed account')
    expect(showWarning).toHaveBeenCalledWith('Token refreshed, but project_id is temporarily unavailable')
    wrapper.unmount()
  })

  it('shows an error and keeps the modal closed when detail loading fails', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    getById.mockRejectedValueOnce(new Error('detail failed'))
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    await editButton!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('detail failed')
    expect(wrapper.get('[data-test="edit-account"]').text()).toBe('')
    consoleError.mockRestore()
    wrapper.unmount()
  })
})
