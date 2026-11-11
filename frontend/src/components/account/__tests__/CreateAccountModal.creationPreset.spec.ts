import { defineComponent, reactive } from 'vue'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AccountCreationPreset } from '@/api/admin/accountCreationPreset'

enableAutoUnmount(afterEach)

const mocks = vi.hoisted(() => ({
  createAccount: vi.fn(), refreshOpenAIToken: vi.fn(), importCodexSession: vi.fn(), createOpenAICodexPAT: vi.fn(),
  getPreset: vi.fn(), savePreset: vi.fn(), createPlan: vi.fn(), createCredentialOperations: vi.fn(),
  showError: vi.fn(), showSuccess: vi.fn(), showWarning: vi.fn(), showInfo: vi.fn(),
  auth: { isAdmin: true, isSimpleMode: true, isObserver: false, user: { id: 1 } }
}))

vi.mock('@/stores/app', () => ({ useAppStore: () => ({
  showError: mocks.showError, showSuccess: mocks.showSuccess, showWarning: mocks.showWarning, showInfo: mocks.showInfo
}) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => reactive(mocks.auth) }))
vi.mock('@/api/admin/accountCreationPreset', () => ({ getAccountCreationPreset: mocks.getPreset, saveAccountCreationPreset: mocks.savePreset }))
vi.mock('@/api/admin/credentialEncryption', () => ({
  getCredentialEncryption: vi.fn().mockResolvedValue({ configured: true, source: 'server_config' }), initializeCredentialEncryption: vi.fn()
}))
vi.mock('@/api/admin/accountTokenGuardV2', () => ({ createTokenGuardV2Account: mocks.createCredentialOperations }))
vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]) }))
vi.mock('@/api/admin', () => ({ adminAPI: {
  accounts: {
    getManagementCapabilities: vi.fn().mockResolvedValue({ web_search_enabled: false, account_quota_notify_enabled: false }),
    create: mocks.createAccount, refreshOpenAIToken: mocks.refreshOpenAIToken,
    importCodexSession: mocks.importCodexSession, createOpenAICodexPAT: mocks.createOpenAICodexPAT,
    checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
  },
  settings: { getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }), getSettings: vi.fn().mockResolvedValue({}) },
  tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }, scheduledTests: { create: mocks.createPlan }
} }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  props: { show: Boolean }, template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})
const OAuthFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: ['error', 'excelOauth', 'showCodexPatOption', 'showAgentIdentityOption', 'showCodexSessionImportOption'],
  data: () => ({ inputMethod: 'manual' }),
  emits: ['import-codex-session', 'import-codex-pat', 'validate-refresh-token'],
  template: '<div data-testid="oauth-flow"><p role="alert" v-if="error">{{ error }}</p></div>'
})
const TwoFAStub = defineComponent({ name: 'OpenAITwoFAImport', props: ['importCredential'], template: '<div data-testid="two-fa-import" />' })
const ModelsStub = defineComponent({ name: 'ModelWhitelistSelector', props: ['modelValue'], emits: ['update:modelValue'], template: '<div />' })

function mountModal() {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups: [] },
    global: { stubs: {
      BaseDialog: BaseDialogStub, OAuthAuthorizationFlow: OAuthFlowStub, OpenAITwoFAImport: TwoFAStub,
      ModelWhitelistSelector: ModelsStub, ConfirmDialog: true, Select: true, Icon: true, PlatformIcon: true,
      ProxySelector: true, ProxyAdBanner: true, GroupSelector: true, QuotaLimitCard: true, UpstreamChannelSelector: true
    } }
  })
}
type Wrapper = ReturnType<typeof mountModal>
type CreationPath = 'create' | 'session' | '2fa'

function preset(patch: Partial<AccountCreationPreset> = {}): AccountCreationPreset {
  return { version: 1, platform: 'openai', type: 'oauth', concurrency: 4, priority: -9, load_factor: null,
    rate_multiplier: 0, group_rate_multiplier: 0, group_ids: [], proxy_id: null, auto_pause_on_expired: true,
    extra: {}, model_mapping: { 'client-model': 'upstream-model' }, ...patch }
}
function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(res => { resolve = res })
  return { promise, resolve }
}
async function button(wrapper: Wrapper, text: string) {
  const found = wrapper.findAll('button').find(candidate => candidate.text().includes(text))
  expect(found, 'button ' + text).toBeDefined()
  await found!.trigger('click')
  await flushPromises()
}
async function openAI(path: CreationPath = 'create') {
  const wrapper = mountModal()
  await button(wrapper, 'OpenAI')
  await wrapper.get('[data-tour="account-form-name"]').setValue('My account')
  if (path === '2fa') await wrapper.get('[data-testid="openai-two-fa"]').trigger('click')
  return wrapper
}
function numberInput(wrapper: Wrapper, label: string) {
  const input = wrapper.findAll('input[type="number"]').find(candidate =>
    candidate.element.parentElement?.querySelector('label')?.textContent?.trim() === label)
  expect(input, 'number input ' + label).toBeDefined()
  return input!
}
async function nextStep(wrapper: Wrapper) {
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await flushPromises()
}
async function finish(wrapper: Wrapper, path: CreationPath = 'create') {
  await nextStep(wrapper)
  if (path === '2fa') {
    const importCredential = wrapper.getComponent(TwoFAStub).props('importCredential')
    await importCredential({ access_token: 'twofa-access', refresh_token: 'twofa-refresh', account_id: 'workspace' },
      'test@example.com', { email: 'test@example.com', password: 'fixture-password', mfa_secret: 'fixture-totp' })
  } else {
    wrapper.getComponent(OAuthFlowStub).vm.$emit(path === 'session' ? 'import-codex-session' : 'validate-refresh-token',
      path === 'session' ? JSON.stringify({ tokens: { access_token: 'session-access' } }) : 'refresh-from-user')
    await flushPromises()
  }
  const api = path === 'create' ? mocks.createAccount : mocks.importCodexSession
  expect(api).toHaveBeenCalledTimes(1)
  return api.mock.calls[0]![0]
}
async function configureBPS(wrapper: Wrapper, allModels = false) {
  await wrapper.get('[data-testid="create-bps-enable"]').setValue(true)
  await wrapper.get('[data-testid="bps-oauth-transport"]').setValue('ws')
  if (allModels) await wrapper.get('[data-testid="create-bps-all-models"]').setValue(true)
  else wrapper.getComponent(ModelsStub).vm.$emit('update:modelValue', ['gpt-6-astra', 'gpt-6-sol'])
  await wrapper.get('[data-testid="create-bps-auto_disable_on_403"]').setValue(true)
  await wrapper.get('[data-testid="create-bps-auto_recover_on_403"]').setValue(true)
  await wrapper.get('[data-testid="create-bps-settings"] input[type="number"]').setValue(15)
  await wrapper.get('[data-testid="create-bps-auto_move_on_403"]').setValue(true)
  await wrapper.get('select[aria-label="admin.accounts.openai.excelBPS403TargetGroup"]').setValue(0)
  await wrapper.get('[data-testid="create-bps-session_proxy"]').setValue(true)
  await wrapper.get('select[aria-label="admin.accounts.openai.excelBPSProxySource"]').setValue('ip_pool')
  await wrapper.get('[data-testid="create-bps-omit_unsupported_tools"]').setValue(true)
  await flushPromises()
}

describe('CreateAccountModal creation configuration', () => {
  beforeEach(() => {
    for (const mock of Object.values(mocks)) if (typeof mock === 'function') mock.mockReset()
    Object.assign(reactive(mocks.auth), { isAdmin: true, isSimpleMode: true, isObserver: false, user: { id: 1 } })
    mocks.createAccount.mockResolvedValue({ id: 81 })
    mocks.refreshOpenAIToken.mockResolvedValue({ access_token: 'access-from-refresh', refresh_token: 'refresh-from-user', expires_in: 3600, email: 'test@example.com' })
    mocks.importCodexSession.mockResolvedValue({ created: 1, updated: 0, skipped: 0, failed: 0, errors: [], warnings: [], items: [{ action: 'created', account_id: 82 }] })
    mocks.getPreset.mockResolvedValue(preset())
    mocks.savePreset.mockImplementation(async value => value)
    mocks.createPlan.mockResolvedValue({ id: 1 })
    mocks.createCredentialOperations.mockResolvedValue({})
  })

  it('shows the admin toolbar without loading or saving automatically', async () => {
    const wrapper = await openAI()
    expect(wrapper.find('[data-testid="account-creation-preset"]').exists()).toBe(true)
    expect(mocks.getPreset).not.toHaveBeenCalled()
    expect(mocks.savePreset).not.toHaveBeenCalled()
    reactive(mocks.auth).isAdmin = false
    await flushPromises()
    expect(wrapper.find('[data-testid="account-creation-preset"]').exists()).toBe(false)
  })

  it.each(['create', 'session', '2fa'] as const)('creates %s accounts with WS, model and 403 BPS settings', async path => {
    const wrapper = await openAI(path)
    await configureBPS(wrapper)
    const request = await finish(wrapper, path)
    expect(request.extra).toMatchObject({ openai_excel_bps: true, openai_excel_bps_transport: 'ws',
      openai_excel_bps_models: ['gpt-6-astra', 'gpt-6-sol'], openai_excel_bps_auto_disable_on_403: true,
      openai_excel_bps_auto_recover_on_403: true, openai_excel_bps_403_recovery_interval_minutes: 15,
      openai_excel_bps_auto_move_on_403: true, openai_excel_bps_403_target_group_id: 0,
      openai_excel_bps_mihomo: true, openai_excel_bps_proxy_source: 'ip_pool', openai_excel_bps_omit_unsupported_tools: true })
    expect(request.auto_pause_on_expired).toBe(true)
    expect(request).not.toHaveProperty('preserve_creation_config')
    expect(mocks.createPlan).not.toHaveBeenCalled()
    if (path === '2fa') expect(request).toMatchObject({ initialize_openai_two_fa: true, update_existing: false, skip_existing: true })
  })

  it('omits the model whitelist when all models are selected', async () => {
    const wrapper = await openAI()
    await configureBPS(wrapper, true)
    expect(wrapper.find('[data-testid="bps-oauth-models"]').exists()).toBe(false)
    const request = await finish(wrapper)
    expect(request.extra.openai_excel_bps).toBe(true)
    expect(request.extra).not.toHaveProperty('openai_excel_bps_models')
  })

  it('forces BPS for standalone Excel OAuth with a checked disabled control', async () => {
    const wrapper = await openAI()
    await wrapper.get('[data-testid="openai-bps-oauth"]').trigger('click')
    const toggle = wrapper.get<HTMLInputElement>('[data-testid="create-bps-enable"]')
    expect(toggle.element.checked).toBe(true)
    expect(toggle.element.disabled).toBe(true)
    expect(wrapper.find('[data-testid="account-auto-bps"]').exists()).toBe(false)
    const request = await finish(wrapper)
    expect(request.extra.openai_excel_bps).toBe(true)
    expect(request.extra).not.toHaveProperty('openai_excel_bps_transport')
    expect(request.credentials.client_id).toBe('app_fnr0pYvVwwFDocDumLG3H2Bp')
  })

  it('hides creation BPS for API keys and omits BPS and preset priority from the payload', async () => {
    const wrapper = await openAI()
    await configureBPS(wrapper)
    await wrapper.get('[data-testid="creation-preset-save"]').trigger('click')
    await flushPromises()
    await button(wrapper, 'API Key')
    expect(wrapper.find('[data-testid="create-bps-settings"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="account-creation-preset"]').exists()).toBe(false)
    await wrapper.get('input[type="password"]').setValue('fixture-api-key')
    await nextStep(wrapper)
    expect(mocks.createAccount).toHaveBeenCalledTimes(1)
    const request = mocks.createAccount.mock.calls[0]![0]
    expect(request).toMatchObject({ platform: 'openai', type: 'apikey', credentials: { api_key: 'fixture-api-key' } })
    expect(Object.keys(request.extra).filter(key => key.startsWith('openai_excel_bps'))).toEqual([])
    expect(request).not.toHaveProperty('preserve_creation_config')
  })

  it('turns off the degradation rule when immediate BPS is enabled', async () => {
    const wrapper = await openAI()
    await wrapper.get('[data-testid="account-auto-bps-toggle"]').trigger('click')
    expect(wrapper.get('[data-testid="account-auto-bps-toggle"]').attributes('aria-checked')).toBe('true')
    await wrapper.get('[data-testid="create-bps-enable"]').setValue(true)
    expect(wrapper.find('[data-testid="account-auto-bps"]').exists()).toBe(false)
    await wrapper.get('[data-testid="create-bps-enable"]').setValue(false)
    expect(wrapper.get('[data-testid="account-auto-bps-toggle"]').attributes('aria-checked')).toBe('false')
  })

  it('clears 403 auto recovery when auto disable is turned off', async () => {
    const wrapper = await openAI()
    await configureBPS(wrapper)
    await wrapper.get('[data-testid="create-bps-auto_disable_on_403"]').setValue(false)
    const recovery = wrapper.get<HTMLInputElement>('[data-testid="create-bps-auto_recover_on_403"]')
    expect(recovery.element.checked).toBe(false)
    expect(recovery.element.disabled).toBe(true)
    const request = await finish(wrapper)
    expect(request.extra.openai_excel_bps_auto_recover_on_403).toBe(false)
  })

  it('explicitly saves only configuration and preserves zeros, negative priority and null load', async () => {
    const wrapper = await openAI('2fa')
    await configureBPS(wrapper)
    await numberInput(wrapper, 'admin.accounts.priority').setValue(-17)
    await numberInput(wrapper, 'admin.accounts.billingRateMultiplier').setValue(0)
    await wrapper.get('[data-testid="create-group-rate-multiplier"]').setValue(0)
    await wrapper.get('[data-testid="account-cost-multiplier"]').setValue(0)
    await wrapper.get('[data-testid="creation-preset-save"]').trigger('click')
    await flushPromises()
    expect(mocks.savePreset).toHaveBeenCalledTimes(1)
    const saved = mocks.savePreset.mock.calls[0]![0]
    expect(saved).toMatchObject({ version: 1, platform: 'openai', type: 'oauth', priority: -17, rate_multiplier: 0,
      group_rate_multiplier: 0, load_factor: null, auto_pause_on_expired: true, extra: { cost_multiplier: 0, openai_excel_bps: true } })
    expect(Object.keys(saved).sort()).toEqual(['version', 'platform', 'type', 'concurrency', 'priority', 'load_factor',
      'rate_multiplier', 'group_rate_multiplier', 'group_ids', 'proxy_id', 'auto_pause_on_expired', 'extra', 'model_mapping'].sort())
    expect(wrapper.get('[data-testid="account-creation-preset"] [role="status"]').text()).toBe('admin.accounts.creationPreset.saved')
    expect(mocks.createAccount).not.toHaveBeenCalled()
    expect(mocks.importCodexSession).not.toHaveBeenCalled()
  })

  it.each(['create', 'session', '2fa'] as const)('omits preserve_creation_config for %s without a successful preset operation', async path => {
    const wrapper = await openAI(path)
    const request = await finish(wrapper, path)
    expect(request).not.toHaveProperty('preserve_creation_config')
    expect(request.extra).not.toHaveProperty('openai_excel_bps')
    if (path === 'create') expect(request.group_rate_multiplier).toBe(1)
  })

  it.each([
    ['load', 'create'], ['save', 'create'], ['load', 'session'], ['save', 'session'], ['load', '2fa'], ['save', '2fa']
  ] as const)('sends preserve_creation_config after %s on the %s path', async (operation, path) => {
    const wrapper = await openAI(path)
    await wrapper.get('[data-testid="creation-preset-' + operation + '"]').trigger('click')
    await flushPromises()
    const request = await finish(wrapper, path)
    expect(request.preserve_creation_config).toBe(true)
    expect(request.name).toBe(path === '2fa' ? 'My account (test@example.com)' : 'My account')
    if (operation === 'load') {
      expect(request).toMatchObject({ concurrency: 4, priority: -9, rate_multiplier: 0, group_rate_multiplier: 0 })
      const credentialExtras = path === 'create' ? request.credentials : request.credential_extras
      expect(credentialExtras.model_mapping).toEqual({ 'client-model': 'upstream-model' })
    } else expect(request.group_rate_multiplier).toBe(1)
    if (path === 'create') expect(request.credentials).toMatchObject({ access_token: 'access-from-refresh', refresh_token: 'refresh-from-user' })
    if (path === 'session') expect(JSON.parse(request.content)).toEqual({ tokens: { access_token: 'session-access' } })
    if (path === '2fa') expect(JSON.parse(request.content)).toMatchObject({ access_token: 'twofa-access', refresh_token: 'twofa-refresh' })
  })

  it('does not reset group rates in an untouched update-existing session import', async () => {
    const wrapper = await openAI('session')
    const request = await finish(wrapper, 'session')
    expect(request.update_existing).toBe(true)
    expect(request).not.toHaveProperty('group_rate_multiplier')
  })

  it('sends an explicitly edited zero group multiplier on session import', async () => {
    const wrapper = await openAI('session')
    await wrapper.get('[data-testid="create-group-rate-multiplier"]').setValue(0)
    const request = await finish(wrapper, 'session')
    expect(request.group_rate_multiplier).toBe(0)
  })

  it('loads a valid preset over an invalid BPS draft without changing the 2FA auth mode', async () => {
    const wrapper = await openAI('2fa')
    await wrapper.get('[data-testid="create-bps-enable"]').setValue(true)
    wrapper.getComponent(ModelsStub).vm.$emit('update:modelValue', [])
    await wrapper.get('[data-testid="creation-preset-load"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="account-creation-preset"] [role="alert"]').exists()).toBe(false)
    expect(wrapper.get('[data-tour="account-form-name"]').element).toHaveProperty('value', 'My account')
    expect(wrapper.get('[data-testid="account-creation-preset"] [role="status"]').text()).toBe('admin.accounts.creationPreset.loaded')
    await finish(wrapper, '2fa')
    expect(mocks.createAccount).not.toHaveBeenCalled()
  })

  it.each(['load', 'save'] as const)('shows a %s API error and omits preset priority from create', async operation => {
    mocks[operation === 'load' ? 'getPreset' : 'savePreset'].mockRejectedValue(new Error('preset backend failed'))
    const wrapper = await openAI()
    await wrapper.get('[data-testid="creation-preset-' + operation + '"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="account-creation-preset"] [role="alert"]').text()).toContain('preset backend failed')
    const request = await finish(wrapper)
    expect(request).not.toHaveProperty('preserve_creation_config')
  })

  it('shows an empty status without applying a missing preset', async () => {
    mocks.getPreset.mockResolvedValue(null)
    const wrapper = await openAI()
    await wrapper.get('[data-testid="creation-preset-load"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="account-creation-preset"] [role="status"]').text()).toBe('admin.accounts.creationPreset.empty')
    expect((await finish(wrapper)).preserve_creation_config).toBeUndefined()
  })

  it.each([
    { version: 2 }, { group_ids: [999] }, { proxy_id: 999 }
  ])('rejects an incompatible or unavailable preset: %j', async patch => {
    mocks.getPreset.mockResolvedValue({ ...preset(), ...patch })
    const wrapper = await openAI()
    const initialPriority = (numberInput(wrapper, 'admin.accounts.priority').element as HTMLInputElement).value
    await wrapper.get('[data-testid="creation-preset-load"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="account-creation-preset"] [role="alert"]').text()).toContain(
      'admin.accounts.creationPreset.' + ('version' in patch ? 'incompatible' : 'unavailableSelection'))
    expect(numberInput(wrapper, 'admin.accounts.priority').element).toHaveProperty('value', initialPriority)
    expect(await finish(wrapper)).not.toHaveProperty('preserve_creation_config')
  })

  it('preserves the forced Excel auth mode when loading a preset with BPS off', async () => {
    const wrapper = await openAI()
    await wrapper.get('[data-testid="openai-bps-oauth"]').trigger('click')
    await wrapper.get('[data-testid="creation-preset-load"]').trigger('click')
    await flushPromises()
    const toggle = wrapper.get<HTMLInputElement>('[data-testid="create-bps-enable"]')
    expect(toggle.element.checked).toBe(true)
    expect(toggle.element.disabled).toBe(true)
    const request = await finish(wrapper)
    expect(request.preserve_creation_config).toBe(true)
    expect(request.extra.openai_excel_bps).toBe(true)
    expect(request.credentials.client_id).toBe('app_fnr0pYvVwwFDocDumLG3H2Bp')
  })

  it('blocks creation and saving for an enabled BPS configuration with no models', async () => {
    const wrapper = await openAI()
    await wrapper.get('[data-testid="create-bps-enable"]').setValue(true)
    wrapper.getComponent(ModelsStub).vm.$emit('update:modelValue', [])
    await flushPromises()
    await nextStep(wrapper)
    expect(mocks.showError).toHaveBeenCalledWith('autoConfig.bps.modelsRequired')
    expect(wrapper.findComponent(OAuthFlowStub).exists()).toBe(false)
    await wrapper.get('[data-testid="creation-preset-save"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="account-creation-preset"] [role="alert"]').text()).toContain('autoConfig.bps.modelsRequired')
    expect(mocks.savePreset).not.toHaveBeenCalled()
    expect(mocks.createAccount).not.toHaveBeenCalled()
  })

  it.each(['close', 'platform', 'identity', 'edit'] as const)('ignores a late preset after %s', async change => {
    const pending = deferred<AccountCreationPreset>()
    mocks.getPreset.mockReturnValue(pending.promise)
    const wrapper = await openAI()
    const initialPriority = (numberInput(wrapper, 'admin.accounts.priority').element as HTMLInputElement).value
    await wrapper.get('[data-testid="creation-preset-load"]').trigger('click')
    expect(wrapper.get('[data-testid="creation-preset-save"]').attributes('disabled')).toBeDefined()
    if (change === 'close') { await wrapper.setProps({ show: false }); await wrapper.setProps({ show: true }); await button(wrapper, 'OpenAI') }
    if (change === 'platform') { await button(wrapper, 'Anthropic'); await button(wrapper, 'OpenAI') }
    if (change === 'identity') { reactive(mocks.auth).user.id = 2; await flushPromises() }
    if (change === 'edit') await numberInput(wrapper, 'admin.accounts.priority').setValue(-21)
    pending.resolve(preset())
    await flushPromises()
    expect(numberInput(wrapper, 'admin.accounts.priority').element).toHaveProperty('value', change === 'edit' ? '-21' : initialPriority)
    expect(wrapper.get('[data-testid="account-creation-preset"]').text()).not.toContain('admin.accounts.creationPreset.applied')
    if (change === 'edit') expect(wrapper.get('[data-testid="account-creation-preset"] [role="status"]').text()).toBe('admin.accounts.creationPreset.changed')
  })

  it('preserves a non-BPS preset when adding a native PAT account', async () => {
    const wrapper = await openAI()
    mocks.createOpenAICodexPAT.mockResolvedValue({ id: 51 })
    await wrapper.get('[data-testid="creation-preset-load"]').trigger('click')
    await flushPromises()
    await nextStep(wrapper)
    wrapper.getComponent(OAuthFlowStub).vm.$emit('import-codex-pat', 'fixture-pat')
    await flushPromises()
    expect(mocks.createOpenAICodexPAT).toHaveBeenCalledTimes(1)
    const request = mocks.createOpenAICodexPAT.mock.calls[0]![0]
    expect(request).toMatchObject({ preserve_creation_config: true, concurrency: 4, priority: -9, group_rate_multiplier: 0 })
    expect(request.extra).not.toHaveProperty('openai_excel_bps')
  })

  it.each(['pat', 'agent'] as const)('hides unsupported %s import and rejects its event before calling the API', async mode => {
    const wrapper = await openAI()
    await wrapper.get('[data-testid="create-bps-enable"]').setValue(true)
    await nextStep(wrapper)
    const flow = wrapper.getComponent(OAuthFlowStub)
    expect(flow.props('showCodexPatOption')).toBe(false)
    expect(flow.props('showAgentIdentityOption')).toBe(false)
    expect(flow.props('showCodexSessionImportOption')).toBe(true)
    if (mode === 'agent') (flow.vm as unknown as { inputMethod: string }).inputMethod = 'agent_identity'
    flow.vm.$emit(mode === 'pat' ? 'import-codex-pat' : 'import-codex-session', mode === 'pat' ? 'fixture-pat' :
      JSON.stringify({ auth_mode: 'agentIdentity', agent_identity: { agent_runtime_id: 'fixture-runtime' } }))
    await flushPromises()
    expect(flow.props('error')).toBe('admin.accounts.creationPreset.unsupportedImport')
    expect(mocks.createOpenAICodexPAT).not.toHaveBeenCalled()
    expect(mocks.importCodexSession).not.toHaveBeenCalled()
    expect(mocks.createAccount).not.toHaveBeenCalled()
  })
})
