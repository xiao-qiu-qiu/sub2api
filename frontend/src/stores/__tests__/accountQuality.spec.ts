import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { reactive } from 'vue'
import { flushPromises } from '@vue/test-utils'
import { useAccountQualityStore } from '../accountQuality'
import { listQualityPlans, listQualityOperations, listQualityTemplates, listQualityPresets } from '@/api/admin/accountQuality'
import { getAllIncludingInactive } from '@/api/admin/groups'
const state = vi.hoisted(() => ({ auth: null as any }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => state.auth }))
vi.mock('@/api/admin/accountQuality', () => ({ listQualityPlans: vi.fn(), listQualityOperations: vi.fn(), listQualityTemplates: vi.fn(), listQualityPresets: vi.fn() }))
vi.mock('@/api/admin/groups', () => ({ getAllIncludingInactive: vi.fn() }))
const deferred = <T>() => { let resolve!: (value: T) => void, reject!: (error: Error) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
beforeEach(() => { setActivePinia(createPinia()); state.auth = reactive({ user: { id: 1, role: 'admin' } }); vi.resetAllMocks(); vi.mocked(listQualityPresets).mockResolvedValue([]); vi.mocked(listQualityPlans).mockResolvedValue([]); vi.mocked(listQualityTemplates).mockResolvedValue([]); vi.mocked(listQualityOperations).mockResolvedValue({ items: [], next_cursor: 0 }); vi.mocked(getAllIncludingInactive).mockResolvedValue([]) })
describe('quality workspace session state', () => {
  it('reloads persisted reusable templates independently of dynamic group rules and clears them on logout', async () => {
    vi.mocked(listQualityPresets).mockResolvedValueOnce([{ id: 17, name: 'Saved check' }] as any)
    const store = useAccountQualityStore(); await store.refresh()
    expect(store.presets.map(item => item.name)).toEqual(['Saved check'])
    expect(store.templates).toEqual([])
    const pending = deferred<any[]>(); vi.mocked(listQualityPresets).mockReturnValueOnce(pending.promise)
    const refresh = store.refreshPresets(); state.auth.user = null
    expect(store.presets).toEqual([])
    pending.resolve([{ id: 18, name: 'Previous session' }]); await refresh
    expect(store.presets).toEqual([])
  })
  it('keeps rule panels usable when template loading fails and can reload on retry', async () => {
    vi.mocked(listQualityPresets).mockRejectedValueOnce(new Error('preset storage unavailable'))
    const store = useAccountQualityStore(); await store.refresh()
    expect(store.rulesLoaded).toBe(true)
    expect(store.presetsError).toBe('preset storage unavailable')
    vi.mocked(listQualityPresets).mockResolvedValueOnce([{ id: 17 }] as any)
    await store.refreshPresets()
    expect(store.presetsError).toBe('')
    expect(store.presets).toHaveLength(1)
  })
  it('loads independent panels concurrently but waits for rules before requesting records', async () => {
    const first = deferred<any[]>(); vi.mocked(listQualityPlans).mockReturnValueOnce(first.promise)
    const store = useAccountQualityStore(); const loading = store.refresh()
    expect(listQualityOperations).not.toHaveBeenCalled(); expect(getAllIncludingInactive).toHaveBeenCalled()
    expect(listQualityPresets).toHaveBeenCalled()
    first.resolve([{ id: 1, account_id: 12, account_name: 'named account' }]); await loading
    expect(listQualityOperations).toHaveBeenCalledTimes(1)
    store.search = 'named'; const next = deferred<any[]>(); vi.mocked(listQualityPlans).mockReturnValueOnce(next.promise)
    const refresh = store.refreshRules(); expect(store.plans[0].account_name).toBe('named account')
    expect(useAccountQualityStore().search).toBe('named')
    next.resolve([]); await refresh
  })
  it('discards a stale pagination response after a newer refresh', async () => {
    const store = useAccountQualityStore(); store.operations = [{ id: 10 }] as any; store.cursor = 10
    const append = deferred<any>(); vi.mocked(listQualityOperations).mockReturnValueOnce(append.promise).mockResolvedValueOnce({ items: [{ id: 20 }] as any, next_cursor: 0 })
    const pending = store.refreshOperations(true); await store.refreshOperations()
    append.resolve({ items: [{ id: 9 }], next_cursor: 9 }); await pending
    expect(store.operations.map(o => o.id)).toEqual([20]); expect(store.cursor).toBe(0)
  })
  it('clears cached data on logout and rejects an old session response', async () => {
    const store = useAccountQualityStore(); store.plans = [{ id: 1 }] as any
    const request = deferred<any[]>(); vi.mocked(listQualityPlans).mockReturnValueOnce(request.promise)
    const pending = store.refreshRules(); state.auth.user = null
    expect(store.plans).toEqual([]); request.resolve([{ id: 99 }]); await pending
    expect(store.plans).toEqual([]); expect(store.rulesLoaded).toBe(false)
  })
  it('loads group rules with the account rules and clears them on logout', async () => {
    vi.mocked(listQualityTemplates).mockResolvedValueOnce([{ id: 3, plan_ids: [1] }] as any)
    const store = useAccountQualityStore(); await store.refreshRules()
    expect(listQualityTemplates).toHaveBeenCalledTimes(1)
    expect(store.templates.map(t => t.id)).toEqual([3])
    state.auth.user = null
    expect(store.templates).toEqual([])
  })
  it('does not clear snapshots when the same user profile is refreshed', () => {
    const store = useAccountQualityStore(); store.plans = [{ id: 1 }] as any
    state.auth.user = { id: 1, role: 'admin', balance: 100 }
    expect(store.plans).toHaveLength(1)
  })
})

describe('quality probe scoped operations', () => {
  const page = (...ids: number[]) => ({ items: ids.map(id => ({ id })) as any, next_cursor: ids[ids.length - 1] ?? 0 })
  const templates = (...ids: number[]) => ids.map(id => ({ id, plan_ids: [id * 10] })) as any

  it('selects the first template on initial rule loading and retains the selection on refresh', async () => {
    vi.mocked(listQualityTemplates).mockResolvedValue(templates(3, 4))
    const store = useAccountQualityStore(); await store.refreshRules()
    expect(store.selectedTemplateId).toBe(3); expect(store.unassignedOnly).toBe(false)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3 })
    await store.selectProbe(4); store.search = 'keep'
    const calls = vi.mocked(listQualityOperations).mock.calls.length
    await store.refreshRules()
    expect(store.selectedTemplateId).toBe(4); expect(store.search).toBe('keep')
    expect(listQualityOperations).toHaveBeenCalledTimes(calls)
  })

  it('keeps the legacy global request when no templates exist', async () => {
    const store = useAccountQualityStore(); await store.refresh()
    expect(store.selectedTemplateId).toBeNull(); expect(store.unassignedOnly).toBe(false)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0)
    expect(listQualityOperations).toHaveBeenCalledTimes(1)
  })

  it('requests the default template exactly once on first refresh without a global fetch', async () => {
    const rules = deferred<any[]>(), records = deferred<any>()
    vi.mocked(listQualityTemplates).mockReturnValueOnce(rules.promise)
    vi.mocked(listQualityOperations).mockReturnValueOnce(records.promise)
    const store = useAccountQualityStore(), pending = store.refresh()
    expect(listQualityOperations).not.toHaveBeenCalled()
    rules.resolve(templates(3)); await flushPromises()
    expect(listQualityOperations).toHaveBeenCalledTimes(1)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3 })
    expect(store.operationsLoading).toBe(true)
    records.resolve(page(31)); await pending
    expect(store.operations.map(item => item.id)).toEqual([31])
    expect(store.cursor).toBe(31)
    expect(listQualityOperations).toHaveBeenCalledTimes(1)
  })

  it('refreshes the existing scope exactly once when rules retain the selection', async () => {
    vi.mocked(listQualityTemplates).mockResolvedValue(templates(3))
    const store = useAccountQualityStore(); await store.refresh()
    vi.mocked(listQualityOperations).mockClear().mockResolvedValueOnce(page(32))
    await store.refresh()
    expect(listQualityOperations).toHaveBeenCalledTimes(1)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3 })
    expect(store.operations.map(item => item.id)).toEqual([32])
  })

  it('does not issue a global request when the session resets while refresh waits for rules', async () => {
    const rules = deferred<any[]>()
    vi.mocked(listQualityTemplates).mockReturnValueOnce(rules.promise)
    const store = useAccountQualityStore(), pending = store.refresh()
    state.auth.user = null; rules.resolve(templates(3)); await pending
    expect(listQualityOperations).not.toHaveBeenCalled()
    expect(store.selectedTemplateId).toBeNull(); expect(store.operations).toEqual([])
  })

  it('clears scope-local controls immediately and ignores an older probe response', async () => {
    const old = deferred<any>(), current = deferred<any>()
    vi.mocked(listQualityOperations).mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise)
    const store = useAccountQualityStore(), first = store.selectProbe(3)
    store.search = 'old search'; store.operationFilter = 'failed'
    store.operations = page(30).items; store.cursor = 30
    const second = store.selectProbe(4)
    expect(store.search).toBe(''); expect(store.operationFilter).toBe('all')
    expect(store.operations).toEqual([]); expect(store.cursor).toBe(0); expect(store.operationsLoaded).toBe(false)
    old.resolve(page(30)); await first
    expect(store.operations).toEqual([]); expect(store.operationsLoading).toBe(true)
    current.resolve(page(40)); await second
    expect(store.operations.map(item => item.id)).toEqual([40]); expect(store.cursor).toBe(40)
    expect(store.operationsLoading).toBe(false)
  })

  it.each(['success', 'failure'])('ignores stale pagination %s after switching probes', async outcome => {
    const oldPage = deferred<any>()
    vi.mocked(listQualityOperations).mockResolvedValueOnce(page(31)).mockReturnValueOnce(oldPage.promise).mockResolvedValueOnce(page(41))
    const store = useAccountQualityStore(); await store.selectProbe(3)
    const pending = store.refreshOperations(true)
    expect(listQualityOperations).toHaveBeenLastCalledWith(31, { template_id: 3 })
    await store.selectProbe(4)
    if (outcome === 'success') oldPage.resolve(page(29))
    else oldPage.reject(new Error('previous scope failed'))
    await pending
    expect(store.operations.map(item => item.id)).toEqual([41]); expect(store.cursor).toBe(41)
    expect(store.operationsError).toBe(''); expect(store.moreLoading).toBe(false)
  })

  it('deduplicates scoped pages and prevents parallel loadMore calls', async () => {
    const next = deferred<any>()
    vi.mocked(listQualityOperations).mockResolvedValueOnce(page(40, 39)).mockReturnValueOnce(next.promise)
    const store = useAccountQualityStore(); await store.selectProbe(4)
    const pending = store.refreshOperations(true); await store.refreshOperations(true)
    expect(listQualityOperations).toHaveBeenCalledTimes(2)
    next.resolve(page(39, 38)); await pending
    expect(store.operations.map(item => item.id)).toEqual([40, 39, 38])
    expect(store.cursor).toBe(38)
  })

  it('requests a directly assigned plan within its scope and rejects the previous page', async () => {
    const old = deferred<any>()
    vi.mocked(listQualityOperations).mockResolvedValueOnce(page(30)).mockReturnValueOnce(old.promise).mockResolvedValueOnce(page(23))
    const store = useAccountQualityStore(); await store.selectProbe(3)
    const pending = store.refreshOperations(true)
    store.selectedPlanId = 21
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3, plan_id: 21 })
    expect(store.operations).toEqual([]); expect(store.cursor).toBe(0)
    await flushPromises(); old.resolve(page(29)); await pending
    expect(store.operations.map(item => item.id)).toEqual([23])
    await store.refreshOperations(true)
    expect(listQualityOperations).toHaveBeenLastCalledWith(23, { template_id: 3, plan_id: 21 })
    store.selectedPlanId = null; await flushPromises()
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3 })
  })

  it('resets the selected plan with exactly one request when choosing standalone or another probe', async () => {
    const store = useAccountQualityStore(); await store.selectProbe(3)
    store.selectedPlanId = 30; await flushPromises()
    const calls = vi.mocked(listQualityOperations).mock.calls.length
    await store.selectProbe(null, true)
    expect(store.selectedPlanId).toBeNull(); expect(store.unassignedOnly).toBe(true)
    expect(listQualityOperations).toHaveBeenCalledTimes(calls + 1)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { unassigned: true })
    store.selectedPlanId = 50; await flushPromises()
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { unassigned: true, plan_id: 50 })
    await store.selectProbe(4, true)
    expect(store.unassignedOnly).toBe(false)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 4 })
    await store.selectProbe(null)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0)
  })

  it('falls back to the first remaining template and then standalone when the selection is deleted', async () => {
    vi.mocked(listQualityTemplates).mockResolvedValueOnce(templates(3, 4)).mockResolvedValueOnce(templates(4)).mockResolvedValueOnce([])
    const store = useAccountQualityStore(); await store.refreshRules()
    store.selectedPlanId = 30; await flushPromises()
    await store.refreshRules()
    expect(store.selectedTemplateId).toBe(4); expect(store.selectedPlanId).toBeNull()
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 4 })
    await store.refreshRules()
    expect(store.selectedTemplateId).toBeNull(); expect(store.unassignedOnly).toBe(true)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { unassigned: true })
  })

  it('clears a deleted selected plan and reloads the template records', async () => {
    vi.mocked(listQualityTemplates).mockResolvedValue(templates(3))
    vi.mocked(listQualityPlans).mockResolvedValueOnce([{ id: 30 }] as any).mockResolvedValueOnce([])
    const store = useAccountQualityStore(); await store.refreshRules()
    store.selectedPlanId = 30; await flushPromises()
    await store.refreshRules(); await flushPromises()
    expect(store.selectedTemplateId).toBe(3); expect(store.selectedPlanId).toBeNull()
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3 })
  })

  it.each(['membership', 'source'])('clears a selected plan when refreshed %s moves it outside the current template', async change => {
    vi.mocked(listQualityTemplates).mockResolvedValue(templates(3, 4))
    vi.mocked(listQualityPlans).mockResolvedValue([{ id: 30, source_template_id: 3 }] as any)
    const store = useAccountQualityStore(); await store.refreshRules()
    store.selectedPlanId = 30; await flushPromises()
    const oldPage = deferred<any>(), records = deferred<any>()
    vi.mocked(listQualityOperations).mockReturnValueOnce(oldPage.promise).mockReturnValueOnce(records.promise)
    store.cursor = 30
    const pageRequest = store.refreshOperations(true)
    const after = templates(3, 4)
    if (change === 'membership') after[0].plan_ids = []
    else vi.mocked(listQualityPlans).mockResolvedValue([{ id: 30, source_template_id: 4 }] as any)
    vi.mocked(listQualityTemplates).mockResolvedValue(after)
    const count = vi.mocked(listQualityOperations).mock.calls.length, refresh = store.refresh()
    await flushPromises()
    expect(store.selectedTemplateId).toBe(3); expect(store.selectedPlanId).toBeNull()
    expect(store.operations).toEqual([]); expect(store.cursor).toBe(0)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3 })
    oldPage.resolve(page(29)); await pageRequest
    expect(store.operations).toEqual([]); expect(store.operationsLoading).toBe(true)
    records.resolve(page(33)); await refresh
    expect(store.operations.map(item => item.id)).toEqual([33])
    expect(listQualityOperations).toHaveBeenCalledTimes(count + 1)
  })

  it('retains an old-API plan selection while the refreshed template still owns the plan', async () => {
    vi.mocked(listQualityTemplates).mockResolvedValue(templates(3))
    vi.mocked(listQualityPlans).mockResolvedValue([{ id: 30 }] as any)
    const store = useAccountQualityStore(); await store.refreshRules()
    store.selectedPlanId = 30; await flushPromises()
    vi.mocked(listQualityOperations).mockClear()
    await store.refresh()
    expect(store.selectedPlanId).toBe(30)
    expect(listQualityOperations).toHaveBeenCalledTimes(1)
    expect(listQualityOperations).toHaveBeenLastCalledWith(0, { template_id: 3, plan_id: 30 })
  })

  it.each(['user', 'role', 'logout'])('resets scopes on %s changes without issuing requests for the old session', async change => {
    const pendingPage = deferred<any>(), pendingRules = deferred<any[]>()
    const store = useAccountQualityStore(); await store.selectProbe(3)
    store.selectedPlanId = 30; await flushPromises()
    store.search = 'old'; store.operationFilter = 'failed'
    vi.mocked(listQualityOperations).mockReturnValueOnce(pendingPage.promise)
    vi.mocked(listQualityTemplates).mockReturnValueOnce(pendingRules.promise)
    const records = store.refreshOperations(), rules = store.refreshRules()
    const calls = vi.mocked(listQualityOperations).mock.calls.length
    state.auth.user = change === 'logout' ? null : change === 'user' ? { id: 2, role: 'admin' } : { id: 1, role: 'user' }
    expect(store.selectedTemplateId).toBeNull(); expect(store.unassignedOnly).toBe(false); expect(store.selectedPlanId).toBeNull()
    expect(store.search).toBe(''); expect(store.operationFilter).toBe('all')
    expect(listQualityOperations).toHaveBeenCalledTimes(calls)
    pendingPage.resolve(page(99)); pendingRules.resolve(templates(9))
    await Promise.all([records, rules])
    expect(store.operations).toEqual([]); expect(store.templates).toEqual([]); expect(store.cursor).toBe(0)
    expect(store.rulesLoaded).toBe(false); expect(store.operationsLoaded).toBe(false)
    expect(store.operationsLoading).toBe(false); expect(store.rulesLoading).toBe(false); expect(store.updatedAt).toBe(0)
  })
})
