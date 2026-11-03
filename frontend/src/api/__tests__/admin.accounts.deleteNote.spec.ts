import { beforeEach, describe, expect, it, vi } from 'vitest'

const { deleteRequest, post } = vi.hoisted(() => ({
  deleteRequest: vi.fn(),
  post: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { delete: deleteRequest, post }
}))

import { batchDelete, deleteAccount } from '@/api/admin/accounts'

describe('admin account deletion notes', () => {
  beforeEach(() => {
    deleteRequest.mockReset()
    post.mockReset()
    deleteRequest.mockResolvedValue({ data: { message: 'deleted' } })
    post.mockResolvedValue({ data: { success_count: 2, failed_count: 0 } })
  })

  it('sends a trimmed note in the single-account DELETE body and keeps internal line breaks', async () => {
    const result = await deleteAccount(7, '  已停用\n后续复查  ')

    expect(deleteRequest).toHaveBeenCalledWith('/admin/accounts/7', {
      data: { delete_note: '已停用\n后续复查' }
    })
    expect(result).toEqual({ message: 'deleted' })
  })

  it('keeps the original single-account request when the note argument is omitted', async () => {
    await deleteAccount(7)

    expect(deleteRequest).toHaveBeenCalledWith('/admin/accounts/7')
  })

  it.each([undefined, '', ' \t\n '])('omits the DELETE body for an empty note: %j', async note => {
    await deleteAccount(7, note)

    expect(deleteRequest).toHaveBeenCalledWith('/admin/accounts/7')
  })

  it('sends one trimmed note with every account in the batch', async () => {
    const result = await batchDelete([7, 8], '  统一归档\n手动删除  ')

    expect(post).toHaveBeenCalledWith('/admin/accounts/batch-delete', {
      account_ids: [7, 8],
      delete_note: '统一归档\n手动删除'
    })
    expect(result).toEqual({ success_count: 2, failed_count: 0 })
  })

  it('keeps the original batch request when the note argument is omitted', async () => {
    await batchDelete([7, 8])

    expect(post).toHaveBeenCalledWith('/admin/accounts/batch-delete', { account_ids: [7, 8] })
  })

  it.each([undefined, '', ' \t\n '])('omits delete_note from a batch with an empty note: %j', async note => {
    await batchDelete([7, 8], note)

    expect(post).toHaveBeenCalledWith('/admin/accounts/batch-delete', { account_ids: [7, 8] })
  })
})
