import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post } }))
import { startManualRest } from '@/api/admin/accounts'

describe('manual rest API duration units', () => {
  beforeEach(() => post.mockReset())

  it.each([undefined, 1, 45, 7200])('sends %s seconds without converting to minutes', async seconds => {
    post.mockResolvedValue({ data: { account_rotation: { phase: 'draining' } } })
    await startManualRest(7, seconds)
    expect(post).toHaveBeenCalledWith('/admin/accounts/7/rotation/rest', { seconds: seconds ?? 60 })
  })
})
