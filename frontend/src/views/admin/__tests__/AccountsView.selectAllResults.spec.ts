import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'
import DeleteAccountDialog from '@/components/admin/account/DeleteAccountDialog.vue'

enableAutoUnmount(afterEach)

const {
  listAccounts,
  listWithEtag,
  batchRefresh,
  batchDelete,
  deleteAccount,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  showError
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  batchRefresh: vi.fn(),
  batchDelete: vi.fn(),
  deleteAccount: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getManagementCapabilities: vi.fn().mockResolvedValue({ web_search_enabled: false, account_quota_notify_enabled: false }),
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      batchDelete,
      delete: deleteAccount,
      batchClearError: vi.fn(),
      batchRefresh,
      bulkUpdate: vi.fn()
    },
    proxies: {
      getAll: getAllProxies
    },
    groups: {
      getAll: getAllGroups
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token'
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const makeAccounts = (count: number) => Array.from({ length: count }, (_, index) => ({
  id: index + 1,
  name: `account-${index + 1}`,
  platform: 'grok',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  created_at: '2026-07-23T00:00:00Z',
  updated_at: '2026-07-23T00:00:00Z'
}))

const AccountBulkActionsBarStub = {
  props: ['selectedIds', 'totalResults', 'selectingAll', 'allResultsSelected'],
  emits: ['select-all-results', 'select-page', 'clear', 'refresh-token', 'delete'],
  template: `
    <div>
      <span data-test="selected-count">{{ selectedIds.length }}</span>
      <span data-test="total-results">{{ totalResults }}</span>
      <span data-test="all-results-selected">{{ String(allResultsSelected) }}</span>
      <button data-test="select-page" @click="$emit('select-page')">select page</button>
      <button data-test="select-all-results" @click="$emit('select-all-results')">select all</button>
      <button data-test="clear" @click="$emit('clear')">clear</button>
      <button data-test="refresh-token" @click="$emit('refresh-token')">refresh token</button>
      <button data-test="delete-accounts" @click="$emit('delete')">delete accounts</button>
    </div>
  `
}

const AccountTableFiltersStub = {
  emits: ['change'],
  template: '<button data-test="change-filter" @click="$emit(\'change\')">change filter</button>'
}

const mountView = () => mount(AccountsView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: {
        template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
      },
      DataTable: {
        props: ['data'],
        template: '<div data-test="data-table"><div v-for="row in data" :key="row.id"><slot name="cell-select" :row="row" /><div data-test="row-actions"><slot name="cell-actions" :row="row" /></div></div></div>'
      },
      Pagination: true,
      ConfirmDialog: true,
      BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
      AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
      AccountTableFilters: AccountTableFiltersStub,
      AccountBulkActionsBar: AccountBulkActionsBarStub,
      AccountActionMenu: true,
      ImportDataModal: true,
      ReAuthAccountModal: true,
      AccountTestModal: true,
      AccountStatsModal: true,
      ScheduledTestsPanel: true,
      SyncFromCrsModal: true,
      TempUnschedStatusModal: true,
      ErrorPassthroughRulesModal: true,
      TLSFingerprintProfilesModal: true,
      CreateAccountModal: true,
      EditAccountModal: true,
      BulkEditAccountModal: true,
      PlatformTypeBadge: true,
      AccountCapacityCell: true,
      AccountStatusIndicator: true,
      AccountTodayStatsCell: true,
      AccountGroupsCell: true,
      AccountUsageCell: true,
      Icon: true
    }
  }
})

describe('admin AccountsView select all filtered results', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset()
    listWithEtag.mockReset()
    batchRefresh.mockReset()
    batchDelete.mockReset()
    deleteAccount.mockReset()
    getBatchTodayStats.mockReset()
    getUpstreamBillingProbeSettings.mockReset()
    getAllProxies.mockReset()
    getAllGroups.mockReset()
    showError.mockReset()

    listWithEtag.mockResolvedValue({
      notModified: true,
      etag: null,
      data: null
    })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: true, interval_minutes: 30 })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('deletes a single account with an optional note and clears canceled notes', async () => {
    listAccounts.mockResolvedValue({ items: makeAccounts(1), total: 1, page: 1, page_size: 20, pages: 1 })
    deleteAccount.mockResolvedValue({ message: 'ok' })
    const wrapper = mountView()
    await flushPromises()
    const deleteButton = () => wrapper.get('[data-test="row-actions"]').findAll('button').find(button => button.text().includes('common.delete'))!
    await deleteButton().trigger('click')
    await flushPromises()
    expect(deleteAccount).not.toHaveBeenCalled()
    await wrapper.get('#account-delete-note').setValue('  配额到期\n暂时停用  ')
    await wrapper.get('#delete-account-form').trigger('submit')
    await flushPromises()
    expect(deleteAccount).toHaveBeenCalledWith(1, '配额到期\n暂时停用')
    expect(wrapper.getComponent(DeleteAccountDialog).props('show')).toBe(false)

    await deleteButton().trigger('click')
    await flushPromises()
    expect(wrapper.get<HTMLTextAreaElement>('#account-delete-note').element.value).toBe('')
    await wrapper.get('#account-delete-note').setValue('取消的备注')
    await wrapper.getComponent(DeleteAccountDialog).get('button[type="button"]').trigger('click')
    await deleteButton().trigger('click')
    await flushPromises()
    expect(wrapper.get<HTMLTextAreaElement>('#account-delete-note').element.value).toBe('')
    await wrapper.get('#delete-account-form').trigger('submit')
    await flushPromises()
    expect(deleteAccount).toHaveBeenLastCalledWith(1, '')
  })

  it('uses one note for the captured batch and retains only failed accounts in selection', async () => {
    listAccounts.mockResolvedValue({ items: makeAccounts(3), total: 3, page: 1, page_size: 20, pages: 1 })
    batchDelete.mockResolvedValue({ total: 3, success: 2, failed: 1, failed_ids: [2] })
    const nativeConfirm = vi.spyOn(window, 'confirm')
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="select-page"]').trigger('click')
    await wrapper.get('[data-test="delete-accounts"]').trigger('click')
    expect(nativeConfirm).not.toHaveBeenCalled()
    expect(batchDelete).not.toHaveBeenCalled()
    expect(wrapper.getComponent(DeleteAccountDialog).props('message')).toBe('admin.accounts.bulkActions.confirmDelete')
    // A selection change behind the dialog cannot change the confirmed target.
    await wrapper.get('[data-test="clear"]').trigger('click')
    await wrapper.get('#account-delete-note').setValue('  同批失效  ')
    await wrapper.get('#delete-account-form').trigger('submit')
    await flushPromises()
    expect(batchDelete).toHaveBeenCalledWith([1, 2, 3], '同批失效')
    expect(wrapper.getComponent(AccountBulkActionsBarStub).props('selectedIds')).toEqual([2])
    expect(showError).toHaveBeenCalledWith('admin.accounts.bulkActions.partialSuccess')
    expect(wrapper.getComponent(DeleteAccountDialog).props('show')).toBe(false)
  })

  it('keeps the note on failure and blocks duplicate submissions while deleting', async () => {
    listAccounts.mockResolvedValue({ items: makeAccounts(2), total: 2, page: 1, page_size: 20, pages: 1 })
    let rejectDelete!: (reason: Error) => void
    batchDelete.mockReturnValueOnce(new Promise((_, reject) => { rejectDelete = reject }))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="select-page"]').trigger('click')
    await wrapper.get('[data-test="delete-accounts"]').trigger('click')
    await wrapper.get('#account-delete-note').setValue('待处理')
    await wrapper.get('#delete-account-form').trigger('submit')
    await wrapper.get('#delete-account-form').trigger('submit')
    expect(batchDelete).toHaveBeenCalledTimes(1)
    expect(wrapper.get<HTMLTextAreaElement>('#account-delete-note').element.disabled).toBe(true)
    wrapper.getComponent(DeleteAccountDialog).vm.$emit('cancel')
    await flushPromises()
    expect(wrapper.getComponent(DeleteAccountDialog).props('show')).toBe(true)
    rejectDelete(new Error('network unavailable'))
    await flushPromises()
    expect(wrapper.get<HTMLTextAreaElement>('#account-delete-note').element.value).toBe('待处理')
    expect(wrapper.getComponent(DeleteAccountDialog).props('busy')).toBe(false)
    batchDelete.mockResolvedValue({ total: 2, success: 2, failed: 0 })
    await wrapper.get('#delete-account-form').trigger('submit')
    await flushPromises()
    expect(batchDelete).toHaveBeenLastCalledWith([1, 2], '待处理')
    expect(wrapper.getComponent(AccountBulkActionsBarStub).props('selectedIds')).toEqual([])
  })

  it.each([
    { name: 'keeps only failed accounts selected', result: { total: 3, success: 2, failed: 1, errors: [{ account_id: 2, error: 'no refresh token available' }] }, expectedIds: [2] },
    { name: 'clears the selection after every account succeeds', result: { total: 3, success: 3, failed: 0 }, expectedIds: [] },
    { name: 'keeps the original selection when failure details are missing', result: { total: 3, success: 2, failed: 1 }, expectedIds: [1, 2, 3] },
  ])('$name after a batch token refresh and table reload', async ({ result, expectedIds }) => {
    listAccounts.mockResolvedValue({ items: makeAccounts(3), total: 3, page: 1, page_size: 20, pages: 1 })
    batchRefresh.mockResolvedValue(result)
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="select-page"]').trigger('click')
    await wrapper.get('[data-test="refresh-token"]').trigger('click')
    await flushPromises()

    expect(batchRefresh).toHaveBeenCalledWith([1, 2, 3])
    expect(listAccounts).toHaveBeenCalledTimes(2)
    expect(wrapper.getComponent(AccountBulkActionsBarStub).props('selectedIds')).toEqual(expectedIds)
    expect(wrapper.findAll<HTMLInputElement>('[data-test="data-table"] input').map(input => input.element.checked))
      .toEqual([1, 2, 3].map(id => expectedIds.includes(id)))
    if (result.failed > 0) {
      expect(showError).toHaveBeenCalledWith('admin.accounts.bulkActions.partialSuccess')
      await wrapper.get('[data-test="refresh-token"]').trigger('click')
      await flushPromises()
      expect(batchRefresh).toHaveBeenLastCalledWith(expectedIds)
    }
    wrapper.unmount()
  })

  it('selects all matching IDs in one commit and clears the selection when filters change', async () => {
    const allAccounts = makeAccounts(45)
    listAccounts.mockImplementation(async (_page: number, pageSize: number) => {
      if (pageSize === 1000) {
        return {
          items: allAccounts,
          total: 45,
          page: 1,
          page_size: 1000,
          pages: 1
        }
      }
      return {
        items: allAccounts.slice(0, 20),
        total: 45,
        page: 1,
        page_size: 20,
        pages: 3
      }
    })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="select-all-results"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('45')
    expect(wrapper.get('[data-test="total-results"]').text()).toBe('45')
    expect(wrapper.get('[data-test="all-results-selected"]').text()).toBe('true')
    expect(listAccounts).toHaveBeenCalledWith(1, 1000, expect.objectContaining({
      lite: '1',
      include_scheduler_score: '0'
    }))

    vi.useFakeTimers()
    await wrapper.get('[data-test="change-filter"]').trigger('click')
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('0')
    expect(wrapper.get('[data-test="all-results-selected"]').text()).toBe('false')
  })

  it('keeps the original page selection when loading all results fails', async () => {
    const currentPage = makeAccounts(20)
    listAccounts.mockImplementation(async (_page: number, pageSize: number) => {
      if (pageSize === 1000) {
        throw new Error('load all failed')
      }
      return {
        items: currentPage,
        total: 45,
        page: 1,
        page_size: 20,
        pages: 3
      }
    })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="select-page"]').trigger('click')
    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('20')

    await wrapper.get('[data-test="select-all-results"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('20')
    expect(wrapper.get('[data-test="all-results-selected"]').text()).toBe('false')
    expect(showError).toHaveBeenCalledWith('admin.accounts.bulkActions.selectAllFailed')
  })
})
