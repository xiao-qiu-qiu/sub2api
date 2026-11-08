import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import { defineComponent, nextTick, ref } from 'vue'
import AccountsView from '../AccountsView.vue'

const { listAccounts, relogin, isRelogging } = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  relogin: vi.fn(),
  isRelogging: vi.fn()
}))

vi.mock('@/composables/useAccountRelogin', () => ({
  useAccountRelogin: () => ({ relogin, isRelogging })
}))

vi.mock('@vueuse/core', async () => ({
  ...await vi.importActual<typeof import('@vueuse/core')>('@vueuse/core'),
  // 页面只测试接线，不启动用量、性能和自动刷新轮询。
  useIntervalFn: () => ({ pause: vi.fn(), resume: vi.fn(), isActive: ref(false) })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag: vi.fn().mockResolvedValue({ notModified: true, etag: null, data: null }),
      getBatchTodayStats: vi.fn().mockResolvedValue({ stats: {} }),
      getManagementCapabilities: vi.fn().mockResolvedValue({}),
      getUpstreamBillingProbeSettings: vi.fn().mockResolvedValue({ enabled: false })
    },
    proxies: { getAll: vi.fn().mockResolvedValue([]) },
    groups: { getAll: vi.fn().mockResolvedValue([]) }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token' })
}))

vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

enableAutoUnmount(afterEach)

const account = { id: 42, name: 'oauth-account', platform: 'openai', type: 'oauth', parent_account_id: null }
const otherAccount = { ...account, id: 43, name: 'other-oauth-account' }
const busyAccountId = ref<number | null>(null)

const MenuStub = defineComponent({
  name: 'AccountActionMenu',
  props: ['show', 'account', 'anchorRect', 'reloginBusy'],
  emits: ['relogin', 'close'],
  template: '<div />'
})

const mountView = () => shallowMount(AccountsView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: { template: '<div><slot name="table" /></div>' },
      DataTable: {
        props: ['data'],
        template: `<div>
          <div v-for="row in data" :key="row.id" :data-account-id="row.id">
            <slot name="cell-actions" :row="row" />
          </div>
        </div>`
      },
      AccountActionMenu: MenuStub
    }
  }
})

async function openMenu(wrapper: ReturnType<typeof mountView>, accountId: number) {
  const button = wrapper.get(`[data-account-id="${accountId}"]`).findAll('button')
    .find(candidate => candidate.text() === 'common.more')
  expect(button).toBeDefined()
  await button!.trigger('click')
  return wrapper.getComponent(MenuStub)
}

describe('AccountsView — 自动重登接线', () => {
  beforeEach(() => {
    localStorage.clear()
    busyAccountId.value = null
    vi.clearAllMocks()
    relogin.mockResolvedValue(undefined)
    isRelogging.mockImplementation((accountId: number) => busyAccountId.value === accountId)
    listAccounts.mockResolvedValue({ items: [account, otherAccount], total: 2, page: 1, page_size: 20, pages: 1 })
  })

  it('菜单 relogin 事件将完整账号传给 composable', async () => {
    const wrapper = mountView()
    await flushPromises()
    const menu = await openMenu(wrapper, account.id)
    const selectedAccount = menu.props('account')
    expect(selectedAccount).toEqual(account)

    menu.vm.$emit('relogin', selectedAccount)
    await flushPromises()

    expect(relogin).toHaveBeenCalledTimes(1)
    expect(relogin).toHaveBeenCalledWith(selectedAccount)
  })

  it('按当前菜单账号计算 reloginBusy，并随忙碌状态和账号切换更新', async () => {
    const wrapper = mountView()
    await flushPromises()
    const menu = await openMenu(wrapper, account.id)

    expect(isRelogging).toHaveBeenCalledWith(account.id)
    expect(menu.props('reloginBusy')).toBe(false)

    busyAccountId.value = account.id
    await nextTick()
    expect(menu.props('reloginBusy')).toBe(true)

    await openMenu(wrapper, otherAccount.id)
    expect(isRelogging).toHaveBeenCalledWith(otherAccount.id)
    expect(menu.props('reloginBusy')).toBe(false)

    busyAccountId.value = otherAccount.id
    await nextTick()
    expect(menu.props('reloginBusy')).toBe(true)

    busyAccountId.value = null
    await nextTick()
    expect(menu.props('reloginBusy')).toBe(false)
  })
})
