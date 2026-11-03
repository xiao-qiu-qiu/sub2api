import { mount } from '@vue/test-utils'
import { reactive } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import { defaultAccountRotationSettings } from '@/api/admin/settings'
import AccountRotationSettings from '../AccountRotationSettings.vue'

vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('account rotation recovery settings', () => {
  it('defaults to 500–2000ms without enabling automatic rotation', () => {
    const settings = defaultAccountRotationSettings()
    expect(settings).toMatchObject({
      enabled: false,
      scheduled_hold_enabled: true,
      early_rest_enabled: false,
      early_rest_window_minutes: 20,
      early_rest_concurrency_threshold: 2,
      early_rest_seconds: 120,
      recovery_min_interval_ms: 500,
      recovery_max_interval_ms: 2000,
    })
    settings.recovery_min_interval_ms = 100
    expect(defaultAccountRotationSettings().recovery_min_interval_ms).toBe(500)
  })

  it('holds scheduled requests by default and preserves the previous drain mode when toggled', async () => {
    const settings = reactive({ ...defaultAccountRotationSettings(), enabled: true, mode: 'sticky' as const })
    const wrapper = mount(AccountRotationSettings, { props: { modelValue: settings } })
    expect(wrapper.find('select').exists()).toBe(false)
    await wrapper.get('#account-rotation-hold-toggle').trigger('click')
    expect(settings.scheduled_hold_enabled).toBe(false)
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('sticky')
    await wrapper.get('#account-rotation-hold-toggle').trigger('click')
    expect(settings.scheduled_hold_enabled).toBe(true)
    expect(settings.mode).toBe('sticky')
    wrapper.unmount()
  })

  it('edits all early-rest parameters and rejects fractional or out-of-range values', async () => {
    const settings = reactive({ ...defaultAccountRotationSettings(), enabled: true })
    const wrapper = mount(AccountRotationSettings, { props: { modelValue: settings } })
    expect(wrapper.find('#account-rotation-early-window').exists()).toBe(false)
    await wrapper.get('#account-rotation-early-toggle').trigger('click')
    for (const [id, key, value, ceiling] of [
      ['window', 'early_rest_window_minutes', 10, 1440],
      ['concurrency', 'early_rest_concurrency_threshold', 3, 10000],
      ['seconds', 'early_rest_seconds', 45, 7200],
    ] as const) {
      const input = wrapper.get<HTMLInputElement>(`#account-rotation-early-${id}`)
      for (const invalid of ['0', '1.5', String(ceiling + 1), '']) {
        await input.setValue(invalid)
        expect(input.element.checkValidity()).toBe(false)
      }
      await input.setValue(String(value))
      expect(input.element.checkValidity()).toBe(true)
      expect(settings[key]).toBe(value)
    }
    wrapper.unmount()
  })

  it('shows editable seconds for manual rest while automatic rotation is off', async () => {
    const settings = reactive(defaultAccountRotationSettings())
    const wrapper = mount(AccountRotationSettings, { props: { modelValue: settings } })
    expect(wrapper.find('select').exists()).toBe(false)
    const min = wrapper.get<HTMLInputElement>('#account-rotation-recovery-min')
    const max = wrapper.get<HTMLInputElement>('#account-rotation-recovery-max')
    expect(min.element.value).toBe('0.5')
    expect(max.element.value).toBe('2')
    expect(min.attributes()).toMatchObject({ type: 'number', min: '0.001', max: '2', step: '0.001' })
    expect(max.attributes()).toMatchObject({ type: 'number', min: '0.5', max: '60', step: '0.001' })
    expect(min.element.checkValidity()).toBe(true)
    expect(max.element.checkValidity()).toBe(true)
    await min.setValue('0.123')
    await max.setValue('1.001')
    expect(settings.recovery_min_interval_ms).toBe(123)
    expect(settings.recovery_max_interval_ms).toBe(1001)
    expect(settings.enabled).toBe(false)
    expect(min.element.checkValidity()).toBe(true)
    expect(max.element.checkValidity()).toBe(true)
    wrapper.unmount()
  })

  it.each([[1, 1], [123, 456], [1, 60000], [60000, 60000]])(
    'displays valid stored intervals %ims–%ims without rounding or invalidating them',
    (minMS, maxMS) => {
      const settings = { ...defaultAccountRotationSettings(), recovery_min_interval_ms: minMS, recovery_max_interval_ms: maxMS }
      const wrapper = mount(AccountRotationSettings, { props: { modelValue: settings } })
      const min = wrapper.get<HTMLInputElement>('#account-rotation-recovery-min')
      const max = wrapper.get<HTMLInputElement>('#account-rotation-recovery-max')
      expect(min.element.valueAsNumber).toBe(minMS / 1000)
      expect(max.element.valueAsNumber).toBe(maxMS / 1000)
      expect(min.element.checkValidity()).toBe(true)
      expect(max.element.checkValidity()).toBe(true)
      wrapper.unmount()
    },
  )

  it('enforces positive intervals, ordered bounds and the 60 second ceiling', async () => {
    const settings = reactive(defaultAccountRotationSettings())
    const wrapper = mount(AccountRotationSettings, { props: { modelValue: settings } })
    const min = wrapper.get<HTMLInputElement>('#account-rotation-recovery-min')
    const max = wrapper.get<HTMLInputElement>('#account-rotation-recovery-max')
    await min.setValue('0')
    expect(min.element.checkValidity()).toBe(false)
    await min.setValue('0.001')
    await max.setValue('60')
    expect(settings.recovery_min_interval_ms).toBe(1)
    expect(settings.recovery_max_interval_ms).toBe(60000)
    expect(min.element.checkValidity()).toBe(true)
    expect(max.element.checkValidity()).toBe(true)
    await max.setValue('60.001')
    expect(max.element.checkValidity()).toBe(false)
    await max.setValue('0.5')
    await min.setValue('0.6')
    expect(min.element.checkValidity()).toBe(false)
    expect(max.element.checkValidity()).toBe(false)
    wrapper.unmount()
  })
})
