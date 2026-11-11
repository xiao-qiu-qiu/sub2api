import { describe, expect, it } from 'vitest'
import { DEFAULT_EXCEL_BPS_MODELS } from '@/constants/account'
import { accountCreationBPSExtra, newAccountCreationBPS, readAccountCreationBPS } from '../accountCreationBPS'

describe('account creation BPS configuration', () => {
  it('starts disabled with independent model lists and optional switches off', () => {
    const first = newAccountCreationBPS()
    const second = newAccountCreationBPS()
    expect(first).toMatchObject({ enabled: false, transport: 'sse', all_models: false,
      ignore_encrypted_content: false, auto_disable_on_403: false, cache_creation_as_input: false })
    expect(first.models).toEqual(DEFAULT_EXCEL_BPS_MODELS)
    first.models.push('private-model')
    expect(second.models).toEqual(DEFAULT_EXCEL_BPS_MODELS)
    expect(DEFAULT_EXCEL_BPS_MODELS).not.toContain('private-model')
  })

  it('omits all BPS extras while disabled, even for an incomplete draft', () => {
    expect(accountCreationBPSExtra({ ...newAccountCreationBPS(), models: [], recovery_interval_minutes: 0 })).toEqual({})
  })

  it('forces BPS for Excel OAuth without mutating the draft', () => {
    const draft = newAccountCreationBPS()
    const extra = accountCreationBPSExtra(draft, true)
    expect(extra).toMatchObject({ openai_excel_bps: true })
    expect(extra).not.toHaveProperty('openai_excel_bps_transport')
    expect(draft.enabled).toBe(false)
  })

  it('serializes WS, normalized models and the existing backend field names', () => {
    const draft = { ...newAccountCreationBPS(), enabled: true, transport: 'ws' as const,
      models: [' gpt-6-astra ', '', 'gpt-6-astra', 'gpt-6-sol'], omit_unsupported_tools: true,
      ignore_encrypted_content: true, auto_disable_on_403: true, auto_recover_on_403: true,
      recovery_interval_minutes: 17, auto_move_on_403: true, target_group_id: 0,
      session_proxy: true, proxy_source: 'ip_pool' as const, cache_creation_as_input: true }
    const originalModels = [...draft.models]
    expect(accountCreationBPSExtra(draft)).toEqual({
      openai_excel_bps: true, openai_excel_bps_transport: 'ws', openai_excel_bps_models: ['gpt-6-astra', 'gpt-6-sol'],
      openai_excel_bps_omit_unsupported_tools: true, openai_excel_bps_ignore_encrypted_content: true,
      openai_excel_bps_auto_disable_on_403: true, openai_excel_bps_auto_recover_on_403: true,
      openai_excel_bps_auto_move_on_403: true, openai_excel_bps_403_target_group_id: 0,
      openai_excel_bps_403_recovery_interval_minutes: 17, openai_excel_bps_mihomo: true,
      openai_excel_bps_proxy_source: 'ip_pool', openai_excel_bps_cache_creation_as_input: true
    })
    expect(draft.models).toEqual(originalModels)
  })

  it('represents all models by omitting the whitelist', () => {
    expect(accountCreationBPSExtra({ ...newAccountCreationBPS(), enabled: true, all_models: true, models: [] }))
      .not.toHaveProperty('openai_excel_bps_models')
  })

  it('disables automatic recovery when automatic disabling is off', () => {
    const extra = accountCreationBPSExtra({ ...newAccountCreationBPS(), enabled: true, auto_recover_on_403: true })
    expect(extra.openai_excel_bps_auto_recover_on_403).toBe(false)
    expect(extra).not.toHaveProperty('openai_excel_bps_403_target_group_id')
    expect(extra).not.toHaveProperty('openai_excel_bps_proxy_source')
  })

  it.each([
    [{ models: [' ', ''] }, 'autoConfig.bps.modelsRequired'],
    [{ recovery_interval_minutes: 0 }, 'admin.accounts.openai.excelBPS403RecoveryIntervalInvalid'],
    [{ recovery_interval_minutes: 1.5 }, 'admin.accounts.openai.excelBPS403RecoveryIntervalInvalid'],
    [{ auto_move_on_403: true, target_group_id: -1 }, 'admin.accounts.openai.excelBPS403SelectTarget'],
    [{ auto_move_on_403: true, target_group_id: 1.5 }, 'admin.accounts.openai.excelBPS403SelectTarget']
  ])('rejects invalid enabled settings: %j', (patch, error) => {
    expect(() => accountCreationBPSExtra({ ...newAccountCreationBPS(), enabled: true, ...patch })).toThrow(error)
    expect(() => accountCreationBPSExtra({ ...newAccountCreationBPS(), ...patch }, true)).toThrow(error)
  })

  it('round trips the persisted BPS settings', () => {
    const draft = { ...newAccountCreationBPS(), enabled: true, transport: 'ws' as const, models: ['gpt-6-astra'],
      auto_disable_on_403: true, auto_recover_on_403: true, recovery_interval_minutes: 9,
      auto_move_on_403: true, target_group_id: 23, session_proxy: true, proxy_source: 'mihomo' as const }
    expect(readAccountCreationBPS(accountCreationBPSExtra(draft))).toEqual(draft)
  })

  it('reads all-models presets, accepts only literal booleans and filters malformed models', () => {
    expect(readAccountCreationBPS({ openai_excel_bps: true, openai_excel_bps_transport: 'ws' }))
      .toMatchObject({ enabled: true, all_models: true, transport: 'ws' })
    expect(readAccountCreationBPS({ openai_excel_bps: 'true', openai_excel_bps_transport: 'invalid',
      openai_excel_bps_models: ['gpt-6-sol', null, 42], openai_excel_bps_mihomo: 1, openai_excel_bps_proxy_source: 'invalid' }))
      .toMatchObject({ enabled: false, all_models: false, transport: 'sse', models: ['gpt-6-sol'], session_proxy: false, proxy_source: 'mihomo' })
  })
})
