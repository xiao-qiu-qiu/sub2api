import { apiClient } from '../client'

export type QualityStatus = 'healthy' | 'degraded' | 'unknown' | 'untested'
export interface QualityHistoryPoint {
  id: number
  plan_id: number
  started_at: string
  status: Exclude<QualityStatus, 'untested'>
  model_id: string
  model_ids?: string[]
  question_kind: string
  quality_action: string
}
export interface QualityOverviewPlan {
  id: number
  model_id: string
  model_ids?: string[]
  enabled: boolean
  running_until: string | null
  next_run_at: string | null
  action: string
  auto_restore: boolean
  test_channel?: 'account' | 'bps'
  recovery_probe_enabled?: boolean
  recovery_probe_interval_minutes?: number
  recovery_confirmation_minutes?: number
}
export interface QualityOverviewAccount {
  account_id: number
  account_name: string
  platform: string
  type: string
  groups: { id: number; name: string }[]
  status: QualityStatus
  healthy_rate: number | null
  history: QualityHistoryPoint[]
  plans: QualityOverviewPlan[]
}
export interface QualityOverviewPage {
  items: QualityOverviewAccount[]
  total: number
  page: number
  page_size: number
}
export async function getQualityOverview(params: { page: number; page_size: number; search?: string; group_id?: number; test_channel: 'account' | 'bps' }, signal?: AbortSignal): Promise<QualityOverviewPage> {
  const { data } = await apiClient.get<QualityOverviewPage>('/admin/account-quality-overview', { params, signal })
  return { ...data, items: data.items ?? [] }
}
