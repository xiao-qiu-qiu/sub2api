import { apiClient } from '../client'
import type { PaginatedResponse } from '@/types'

export type TrashKind = 'proxies' | 'accounts'

export interface TrashEntry {
  id: number
  name: string
  deleted_at: string
  status?: string
  protocol?: string
  host?: string
  port?: number
  platform?: string
  type?: string
  login_email?: string
  has_login_credentials?: boolean
}

export async function listTrash(
  kind: TrashKind,
  page: number = 1,
  pageSize: number = 20,
  search: string = '',
  signal?: AbortSignal
): Promise<PaginatedResponse<TrashEntry>> {
  const { data } = await apiClient.get<PaginatedResponse<TrashEntry>>(`/admin/${kind}/trash`, {
    params: { page, page_size: pageSize, search },
    signal
  })
  return data
}

export async function restoreTrash(kind: TrashKind, id: number): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>(`/admin/${kind}/${id}/restore`)
  return data
}

export async function permanentlyDeleteTrash(
  kind: TrashKind,
  id: number
): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/${kind}/${id}/permanent`)
  return data
}
