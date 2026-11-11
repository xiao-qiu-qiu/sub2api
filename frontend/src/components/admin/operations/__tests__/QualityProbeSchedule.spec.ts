import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, ref } from 'vue'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import QualityProbeSchedule from '../QualityProbeSchedule.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

enableAutoUnmount(afterEach)

function mountSchedule(initial = '*/2 * * * *', native = true) {
  return mount(defineComponent({
    components: { QualityProbeSchedule },
    setup: () => ({ cron: ref(initial), native: ref(native) }),
    template: '<QualityProbeSchedule v-model="cron" :native="native" />',
  }))
}

describe('QualityProbeSchedule', () => {
  it.each([
    [1, '* * * * *'], [2, '*/2 * * * *'], [5, '*/5 * * * *'], [10, '*/10 * * * *'],
    [15, '*/15 * * * *'], [30, '*/30 * * * *'], [60, '0 * * * *'],
    [120, '0 */2 * * *'], [360, '0 */6 * * *'], [720, '0 */12 * * *'], [1440, '0 0 * * *'],
  ])('allows selecting a %s-minute interval for an answer probe without a native-channel hint', async (_minutes, cron) => {
    const wrapper = mountSchedule('*/2 * * * *', false)
    await wrapper.get('select').setValue(cron)
    expect(wrapper.vm.cron).toBe(cron)
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe(cron)
    expect(wrapper.find('[data-testid="quality-probe-cron"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('qualityOps.probeIntervalHint')
  })

  it('shows the native-channel hint only for native probes', async () => {
    const wrapper = mountSchedule()
    expect(wrapper.text()).toContain('qualityOps.probeIntervalHint')
    wrapper.vm.native = false
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).not.toContain('qualityOps.probeIntervalHint')
  })

  it('shows the two-minute preset and updates the schedule on selection', async () => {
    const wrapper = mountSchedule()
    const select = wrapper.get<HTMLSelectElement>('[data-testid="quality-probe-interval"]')
    expect(select.element.value).toBe('*/2 * * * *')
    expect(wrapper.find('[data-testid="quality-probe-cron"]').exists()).toBe(false)
    await select.setValue('0 */2 * * *')
    expect(wrapper.vm.cron).toBe('0 */2 * * *')
    await select.setValue('0 0 * * *')
    expect(wrapper.vm.cron).toBe('0 0 * * *')
  })

  it('reads custom schedules without changing them and supports editing or returning to a preset', async () => {
    const wrapper = mountSchedule('13 9 * * 1-5')
    expect(wrapper.vm.cron).toBe('13 9 * * 1-5')
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('custom')
    const cron = wrapper.get<HTMLInputElement>('[data-testid="quality-probe-cron"]')
    expect(cron.element.value).toBe('13 9 * * 1-5')
    await cron.setValue(' 7 8 * * 1 ')
    expect(wrapper.vm.cron).toBe('7 8 * * 1')
    await wrapper.get('select').setValue('*/5 * * * *')
    expect(wrapper.vm.cron).toBe('*/5 * * * *')
    expect(wrapper.find('[data-testid="quality-probe-cron"]').exists()).toBe(false)
  })

  it('keeps the existing expression when entering custom mode', async () => {
    const wrapper = mountSchedule('*/30 * * * *')
    await wrapper.get('select').setValue('custom')
    expect(wrapper.vm.cron).toBe('*/30 * * * *')
    await wrapper.get('[data-testid="quality-probe-cron"]').setValue('*/2 * * * *')
    expect(wrapper.get<HTMLSelectElement>('select').element.value).toBe('custom')
    expect(wrapper.find('[data-testid="quality-probe-cron"]').exists()).toBe(true)
  })
})
