import { apiClient } from '../client'

export interface AccountCreationPreset {
  version: 1
  platform: 'openai'
  type: 'oauth'
  concurrency: number
  priority: number
  load_factor: number | null
  rate_multiplier: number
  group_rate_multiplier: number
  group_ids: number[]
  proxy_id: number | null
  auto_pause_on_expired: boolean
  extra: Record<string, unknown>
  model_mapping: Record<string, string>
}

export async function getAccountCreationPreset(): Promise<AccountCreationPreset | null> {
  return (await apiClient.get('/admin/account-ops/creation-preset')).data
}
export async function saveAccountCreationPreset(preset: AccountCreationPreset): Promise<AccountCreationPreset> {
  return (await apiClient.put('/admin/account-ops/creation-preset', preset)).data
}
