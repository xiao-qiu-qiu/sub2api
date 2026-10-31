import { describe, expect, it } from 'vitest'
import { codexFingerprintSettingsPatch, readCodexFingerprintSettings, validateCodexFingerprintSettings } from '../codexFingerprintSettings'

describe('Codex advanced fingerprint settings', () => {
  it('preserves missing defaults without opting into v2 or wire flags', () => {
    const state = readCodexFingerprintSettings()
    expect(state.scheme).toBe('v1')
    expect(Object.values(state.flags).every(value => value === false)).toBe(true)
    expect(codexFingerprintSettingsPatch(state)).toEqual({})
  })

  it('round trips existing settings and preserves unrelated extra and future wire keys', () => {
    const extra = {
      unrelated: { keep: true }, codex_identity_scheme: 'v2',
      codex_wire_options: { cookie_replay: true, future_flag: true },
      codex_wire_timezone: 'Asia/Shanghai',
      codex_wire_user_location: { city: 'Shanghai', region: '', country: 'CN', timezone: '' },
      codex_client_profile: { client: 'desktop', platform: 'windows', versionMode: 'latest', desktopVersion: '2026.11.1' }
    }
    const state = readCodexFingerprintSettings(extra)
    expect(codexFingerprintSettingsPatch(state, extra)).toEqual({})
    state.flags.body_order = true
    const patch = codexFingerprintSettingsPatch(state, extra)
    expect(Object.keys(patch)).toEqual(['codex_wire_options'])
    expect(patch.codex_wire_options).toMatchObject({ body_order: true, cookie_replay: true, future_flag: true })
    expect({ ...extra, ...patch }.unrelated).toEqual({ keep: true })
    expect(extra.codex_wire_options).not.toHaveProperty('body_order')
  })

  it('switches scheme independently and explicitly resets bulk defaults', () => {
    const state = readCodexFingerprintSettings()
    state.scheme = 'v2'
    expect(codexFingerprintSettingsPatch(state)).toEqual({ codex_identity_scheme: 'v2' })
    const reset = codexFingerprintSettingsPatch(readCodexFingerprintSettings(), undefined, true)
    expect(reset).toMatchObject({ codex_identity_scheme: 'v1', codex_wire_timezone: null, codex_wire_user_location: null, codex_client_profile: null })
    expect(Object.values(reset.codex_wire_options as object).every(value => value === false)).toBe(true)
  })

  it('clears overrides explicitly without modifying the original account', () => {
    const extra = { codex_wire_timezone: 'UTC', codex_wire_user_location: { city: 'Paris' }, codex_client_profile: { client: 'cli', platform: 'linux', versionMode: 'latest' } }
    const state = readCodexFingerprintSettings(extra)
    state.timezone = ''
    state.location.city = ''
    state.client = 'inherit'
    expect(codexFingerprintSettingsPatch(state, extra)).toEqual({ codex_wire_timezone: null, codex_wire_user_location: null, codex_client_profile: null })
    expect(extra.codex_wire_user_location.city).toBe('Paris')
  })

  it('requires independent core and Desktop versions and validates manual timezones', () => {
    const state = readCodexFingerprintSettings()
    state.client = 'desktop'
    expect(validateCodexFingerprintSettings(state)).toMatch(/desktopVersionRequired$/)
    state.desktopVersion = '2026.11.1'
    state.versionMode = 'fixed'
    expect(validateCodexFingerprintSettings(state)).toMatch(/coreVersionRequired$/)
    state.codexVersion = '0.100.0'
    expect(validateCodexFingerprintSettings(state)).toBeNull()
    state.timezone = 'Not/AZone'
    expect(validateCodexFingerprintSettings(state)).toMatch(/invalidTimezone$/)
    state.timezone = 'Asia/Shanghai'
    state.location.timezone = 'Not/AZone'
    expect(validateCodexFingerprintSettings(state)).toMatch(/invalidTimezone$/)
  })
})
