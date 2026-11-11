import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, ref } from 'vue'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import QualityProbeTabs, { type ProbeTab } from '../QualityProbeTabs.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({
  t: (key: string, params: Record<string, unknown> = {}) => key + (Object.values(params).length ? ': ' + Object.values(params).join(', ') : ''),
}) }))

enableAutoUnmount(afterEach)

const probes: ProbeTab[] = [7, 8, 9].map((id, index) => ({
  id, name: 'Probe ' + id, icon: 'play', channel: 'Channel ' + id, schedule: 'Every ' + id + ' minutes',
  count: index + 1, status: 'Status ' + id, warning: index === 1, paused: index === 2,
}))

function mountTabs(standaloneCount = 0, disabled = false, items = probes) {
  return mount(defineComponent({
    components: { QualityProbeTabs },
    setup: () => ({ probes: items, selected: ref<number | null>(items[0]?.id ?? null), standaloneCount, disabled }),
    template: '<QualityProbeTabs :probes="probes" :selected-id="selected" :standalone-selected="selected === null" :standalone-count="standaloneCount" :disabled="disabled" @select="selected = $event" />',
  }), { attachTo: document.body })
}

describe('QualityProbeTabs', () => {
  it('renders each probe with its own metadata and one selected keyboard tab stop', async () => {
    const wrapper = mountTabs()
    expect(wrapper.get('[role="tablist"]').attributes('aria-label')).toBe('qualityOps.probeSwitcher')
    expect(wrapper.findAll('[role="tab"]')).toHaveLength(3)
    for (const probe of probes) {
      const tab = wrapper.get('[data-probe-id="' + probe.id + '"]')
      expect(tab.text()).toContain(probe.name)
      expect(tab.text()).toContain(probe.channel)
      expect(tab.text()).toContain(probe.schedule)
      expect(tab.text()).toContain('qualityOps.probeAccountCount: ' + probe.count)
      expect(tab.attributes('aria-controls')).toBe('quality-probe-panel')
      expect(tab.attributes('aria-selected')).toBe(String(probe.id === 7))
      expect(tab.attributes('tabindex')).toBe(probe.id === 7 ? '0' : '-1')
    }
    await wrapper.get('[data-probe-id="8"]').trigger('click')
    expect(wrapper.vm.selected).toBe(8)
    expect(wrapper.get('[data-probe-id="8"]').attributes('tabindex')).toBe('0')
    expect(wrapper.get('[data-probe-id="7"]').attributes('tabindex')).toBe('-1')
    expect(wrapper.getComponent(QualityProbeTabs).emitted('configure')).toBeUndefined()
  })

  it.each([7, 8, 9])('opens probe %s settings from its button and context menu without selecting it', async id => {
    const wrapper = mountTabs()
    const settings = wrapper.get('[data-probe-configure="' + id + '"]')
    expect(settings.element.tagName).toBe('BUTTON')
    expect(settings.attributes('aria-label')).toBe('qualityOps.configureProbeName: Probe ' + id)
    await settings.trigger('click')
    const contextMenu = new MouseEvent('contextmenu', { bubbles: true, cancelable: true })
    wrapper.get('[data-probe-id="' + id + '"]').element.dispatchEvent(contextMenu)
    expect(contextMenu.defaultPrevented).toBe(true)
    expect(wrapper.getComponent(QualityProbeTabs).emitted('configure')).toEqual([[id], [id]])
    expect(wrapper.vm.selected).toBe(7)
  })

  it('moves selection and focus with arrows, wraps, and supports Home and End', async () => {
    const wrapper = mountTabs(2)
    const navigate = async (from: number | string, key: string, to: number | string) => {
      await wrapper.get('[data-probe-id="' + from + '"]').trigger('keydown', { key })
      const target = wrapper.get('[data-probe-id="' + to + '"]')
      expect(target.attributes('aria-selected')).toBe('true')
      expect(target.attributes('tabindex')).toBe('0')
      expect(document.activeElement).toBe(target.element)
      expect(wrapper.findAll('[role="tab"][tabindex="0"]')).toHaveLength(1)
    }
    await navigate(7, 'ArrowRight', 8)
    await navigate(8, 'ArrowRight', 9)
    await navigate(9, 'ArrowRight', 'standalone')
    await navigate('standalone', 'ArrowRight', 7)
    await navigate(7, 'ArrowLeft', 'standalone')
    await navigate('standalone', 'Home', 7)
    await navigate(7, 'End', 'standalone')
    expect(wrapper.vm.selected).toBeNull()
  })

  it.each([{ key: 'ContextMenu' }, { key: 'F10', shiftKey: true }])('opens keyboard settings with $key without changing the selected probe', async event => {
    const wrapper = mountTabs()
    await wrapper.get('[data-probe-id="9"]').trigger('keydown', event)
    expect(wrapper.getComponent(QualityProbeTabs).emitted('configure')).toEqual([[9]])
    expect(wrapper.vm.selected).toBe(7)
  })

  it('leaves Tab navigation to the browser and retains focus on the current tab', () => {
    const wrapper = mountTabs()
    const tab = wrapper.get('[data-probe-id="7"]')
    ;(tab.element as HTMLButtonElement).focus()
    const event = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true })
    tab.element.dispatchEvent(event)
    expect(event.defaultPrevented).toBe(false)
    expect(wrapper.getComponent(QualityProbeTabs).emitted('select')).toBeUndefined()
    expect(wrapper.getComponent(QualityProbeTabs).emitted('configure')).toBeUndefined()
    expect(document.activeElement).toBe(tab.element)
  })

  it('offers standalone rules when no templates exist', async () => {
    const wrapper = mountTabs(0, false, [])
    const tab = wrapper.get('[data-probe-id="standalone"]')
    expect(tab.attributes('aria-selected')).toBe('true')
    expect(tab.attributes('tabindex')).toBe('0')
    await tab.trigger('keydown', { key: 'ArrowRight' })
    expect(document.activeElement).toBe(tab.element)
    await tab.trigger('keydown', { key: 'ContextMenu' })
    expect(wrapper.getComponent(QualityProbeTabs).emitted('configure')).toBeUndefined()
  })

  it('disables selection and settings while work is pending', async () => {
    const wrapper = mountTabs(1, true)
    for (const button of wrapper.findAll('button')) {
      expect((button.element as HTMLButtonElement).disabled).toBe(true)
      await button.trigger('click')
    }
    await wrapper.get('[data-probe-id="8"]').trigger('contextmenu')
    expect(wrapper.getComponent(QualityProbeTabs).emitted('select')).toBeUndefined()
    expect(wrapper.getComponent(QualityProbeTabs).emitted('configure')).toBeUndefined()
  })
})
