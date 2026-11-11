import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../../client'
import { listQualityOperations, listQualityPlans, listQualityTemplates } from '../accountQuality'

vi.mock('../../client', () => ({ apiClient: { get: vi.fn() } }))
beforeEach(() => vi.mocked(apiClient.get).mockReset().mockResolvedValue({ data: {} }))

describe('quality operation scope API', () => {
  it('preserves the old unfiltered request and empty pagination defaults', async () => {
    expect(await listQualityOperations()).toEqual({ items: [], next_cursor: 0 })
    expect(apiClient.get).toHaveBeenLastCalledWith('/admin/account-quality-results', { params: { before_id: 0 } })
  })

  it('sends template and plan filters together before pagination and preserves the server cursor', async () => {
    const page = { items: [{ id: 12, plan_id: 5 }], next_cursor: 8 }
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: page })
    const filter = { template_id: 3, plan_id: 5 }
    expect(await listQualityOperations(20, filter)).toEqual(page)
    expect(apiClient.get).toHaveBeenLastCalledWith('/admin/account-quality-results', { params: { before_id: 20, template_id: 3, plan_id: 5 } })
    expect(filter).toEqual({ template_id: 3, plan_id: 5 })
  })

  it.each([true, false])('passes the explicit unassigned=%s filter without coercion', async unassigned => {
    await listQualityOperations(17, { unassigned })
    expect(apiClient.get).toHaveBeenLastCalledWith('/admin/account-quality-results', { params: { before_id: 17, unassigned } })
  })

  it('accepts old template and plan responses without diagnostics or source identity', async () => {
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: [{ id: 3, plan_ids: [5] }] }).mockResolvedValueOnce({ data: [{ id: 5 }] })
    expect(await listQualityTemplates()).toEqual([{ id: 3, plan_ids: [5] }])
    expect(await listQualityPlans()).toEqual([{ id: 5 }])
  })

  it('retains optional diagnostics and source identity from the new API', async () => {
    const diagnostics = { matched_accounts: 5, active_plans: 2, suspended_plans: 1, excluded_accounts: 1,
      unsupported_accounts: 1, conflicting_accounts: 0, pending_accounts: 1, failed_accounts: 0,
      status: 'partial', reasons: { unsupported_account: 1 } }
    const template = { id: 3, plan_ids: [5], diagnostics }, plan = { id: 5, source_template_id: 3 }
    vi.mocked(apiClient.get).mockResolvedValueOnce({ data: [template] }).mockResolvedValueOnce({ data: [plan] })
    expect(await listQualityTemplates()).toEqual([template])
    expect(await listQualityPlans()).toEqual([plan])
  })
})
