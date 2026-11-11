import { initialExcelBPSDefaults, excelBPSDefaultsError, type ExcelBPSDefaults } from './excelBPSDefaults'
import { excelBPSTransport, type ExcelBPSTransport } from './excelBPSTransport'

export interface AccountCreationBPS extends ExcelBPSDefaults {
  enabled: boolean
  transport: ExcelBPSTransport
}

export function newAccountCreationBPS(): AccountCreationBPS {
  return { ...initialExcelBPSDefaults(), enabled: false, transport: 'sse' }
}

const booleanKeys = {
  omit_unsupported_tools: 'openai_excel_bps_omit_unsupported_tools',
  ignore_encrypted_content: 'openai_excel_bps_ignore_encrypted_content',
  auto_disable_on_403: 'openai_excel_bps_auto_disable_on_403',
  auto_recover_on_403: 'openai_excel_bps_auto_recover_on_403',
  auto_move_on_403: 'openai_excel_bps_auto_move_on_403',
  session_proxy: 'openai_excel_bps_mihomo',
  cache_creation_as_input: 'openai_excel_bps_cache_creation_as_input'
} as const

export function accountCreationBPSExtra(draft: AccountCreationBPS, forceEnabled = false): Record<string, unknown> {
  if (!draft.enabled && !forceEnabled) return {}
  const error = excelBPSDefaultsError(draft)
  if (error) throw new Error(error)
  const extra: Record<string, unknown> = { openai_excel_bps: true }
  if (draft.transport === 'ws') extra.openai_excel_bps_transport = 'ws'
  if (!draft.all_models) extra.openai_excel_bps_models = [...new Set(draft.models.map(model => model.trim()).filter(Boolean))]
  for (const [key, field] of Object.entries(booleanKeys)) extra[field] = draft[key as keyof typeof booleanKeys]
  extra.openai_excel_bps_auto_recover_on_403 = draft.auto_disable_on_403 && draft.auto_recover_on_403
  extra.openai_excel_bps_403_recovery_interval_minutes = draft.recovery_interval_minutes
  if (draft.auto_move_on_403) extra.openai_excel_bps_403_target_group_id = draft.target_group_id
  if (draft.session_proxy) extra.openai_excel_bps_proxy_source = draft.proxy_source
  return extra
}

export function readAccountCreationBPS(extra: Record<string, unknown>): AccountCreationBPS {
  const draft = newAccountCreationBPS()
  draft.enabled = extra.openai_excel_bps === true
  draft.transport = excelBPSTransport(extra.openai_excel_bps_transport)
  draft.all_models = draft.enabled && !Array.isArray(extra.openai_excel_bps_models)
  if (Array.isArray(extra.openai_excel_bps_models)) draft.models = extra.openai_excel_bps_models.filter((model): model is string => typeof model === 'string')
  for (const [key, field] of Object.entries(booleanKeys)) draft[key as keyof typeof booleanKeys] = extra[field] === true
  if (typeof extra.openai_excel_bps_403_recovery_interval_minutes === 'number') draft.recovery_interval_minutes = extra.openai_excel_bps_403_recovery_interval_minutes
  if (typeof extra.openai_excel_bps_403_target_group_id === 'number') draft.target_group_id = extra.openai_excel_bps_403_target_group_id
  draft.proxy_source = extra.openai_excel_bps_proxy_source === 'ip_pool' ? 'ip_pool' : 'mihomo'
  return draft
}
