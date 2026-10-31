export const codexWireFlags = [
  'body_order', 'request_compression', 'environment_timezone', 'web_search_location',
  'settings_user', 'guardian_credits', 'cookie_replay'
] as const

export type CodexWireFlag = typeof codexWireFlags[number]
export interface CodexFingerprintSettings {
  scheme: 'v1' | 'v2'
  flags: Record<CodexWireFlag, boolean>
  timezone: string
  location: { city: string; region: string; country: string; timezone: string }
  client: 'inherit' | 'cli' | 'desktop'
  platform: 'macos' | 'windows' | 'linux'
  versionMode: 'latest' | 'fixed'
  codexVersion: string
  desktopVersion: string
  cliEntry: boolean
  osType: string
  osVersion: string
  arch: string
  terminal: string
}

const object = (value: unknown): Record<string, unknown> =>
  value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
const string = (value: unknown): string => typeof value === 'string' ? value : ''

export function readCodexFingerprintSettings(extra?: Record<string, unknown> | null): CodexFingerprintSettings {
  const wire = object(extra?.codex_wire_options)
  const location = object(extra?.codex_wire_user_location)
  const profile = object(extra?.codex_client_profile)
  return {
    scheme: extra?.codex_identity_scheme === 'v2' ? 'v2' : 'v1',
    flags: Object.fromEntries(codexWireFlags.map(key => [key, wire[key] === true])) as Record<CodexWireFlag, boolean>,
    timezone: string(extra?.codex_wire_timezone),
    location: { city: string(location.city), region: string(location.region), country: string(location.country), timezone: string(location.timezone) },
    client: profile.client === 'cli' || profile.client === 'desktop' ? profile.client : 'inherit',
    platform: profile.platform === 'windows' || profile.platform === 'linux' ? profile.platform : 'macos',
    versionMode: profile.versionMode === 'fixed' ? 'fixed' : 'latest',
    codexVersion: string(profile.codexVersion),
    desktopVersion: string(profile.desktopVersion),
    cliEntry: profile.cliEntry === 'exec',
    osType: string(profile.osType), osVersion: string(profile.osVersion),
    arch: string(profile.arch), terminal: string(profile.terminal)
  }
}

function serialize(settings: CodexFingerprintSettings): Record<string, unknown> {
  const location = Object.fromEntries(Object.entries(settings.location).map(([key, value]) => [key, value.trim()]))
  const profile: Record<string, unknown> = {
    client: settings.client, platform: settings.platform, versionMode: settings.versionMode
  }
  if (settings.versionMode === 'fixed') profile.codexVersion = settings.codexVersion.trim()
  if (settings.client === 'desktop') profile.desktopVersion = settings.desktopVersion.trim()
  if (settings.client === 'cli' && settings.cliEntry) profile.cliEntry = 'exec'
  for (const key of ['osType', 'osVersion', 'arch', 'terminal'] as const) {
    if (settings[key].trim()) profile[key] = settings[key].trim()
  }
  return {
    codex_identity_scheme: settings.scheme,
    codex_wire_options: { ...settings.flags },
    codex_wire_timezone: settings.timezone.trim() || null,
    codex_wire_user_location: Object.values(location).some(Boolean) ? location : null,
    codex_client_profile: settings.client === 'inherit' ? null : profile
  }
}

// Single-account edits send only changed keys. Bulk applies the explicitly selected
// settings as a complete group; null resets optional overrides in a JSONB merge.
export function codexFingerprintSettingsPatch(
  settings: CodexFingerprintSettings,
  base?: Record<string, unknown> | null,
  applyAll = false
): Record<string, unknown> {
  const previous = serialize(readCodexFingerprintSettings(base))
  const patch: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(serialize(settings))) {
    if (applyAll || JSON.stringify(value) !== JSON.stringify(previous[key])) patch[key] = value
  }
  if (patch.codex_wire_options) {
    patch.codex_wire_options = { ...object(base?.codex_wire_options), ...object(patch.codex_wire_options) }
  }
  return patch
}

function validTimezone(value: string): boolean {
  if (!value.trim()) return true
  try {
    new Intl.DateTimeFormat('en', { timeZone: value.trim() })
    return true
  } catch {
    return false
  }
}

export function validateCodexFingerprintSettings(
  settings: CodexFingerprintSettings,
  base?: Record<string, unknown> | null,
  applyAll = false
): string | null {
  const patch = codexFingerprintSettingsPatch(settings, base, applyAll)
  const prefix = 'admin.accounts.openai.fingerprintAdvanced.'
  if (patch.codex_client_profile) {
    if (settings.versionMode === 'fixed' && !settings.codexVersion.trim()) return prefix + 'coreVersionRequired'
    if (settings.client === 'desktop' && !settings.desktopVersion.trim()) return prefix + 'desktopVersionRequired'
  }
  if (('codex_wire_timezone' in patch && !validTimezone(settings.timezone)) ||
      ('codex_wire_user_location' in patch && !validTimezone(settings.location.timezone))) return prefix + 'invalidTimezone'
  return null
}
