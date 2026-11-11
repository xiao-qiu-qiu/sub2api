<template>
  <div class="probe-tabs" role="tablist" :aria-label="t('qualityOps.probeSwitcher')" data-testid="quality-probe-tabs">
    <div v-for="probe in probes" :key="probe.id" class="probe-tab" :class="{ selected: selectedId === probe.id }" @contextmenu.prevent="!disabled && emit('configure', probe.id)">
      <button :id="`quality-probe-${probe.id}`" class="probe-tab-select" role="tab" :aria-selected="selectedId === probe.id" aria-controls="quality-probe-panel" :tabindex="selectedId === probe.id ? 0 : -1" :disabled="disabled" :data-probe-id="probe.id" @click="emit('select', probe.id)" @keydown="navigate($event, probe.id)">
        <span class="probe-tab-heading"><Icon :name="probe.icon" size="sm" /><strong>{{ probe.name }}</strong><span class="probe-tab-state" :class="probe.warning ? 'state-warning' : probe.paused ? 'state-paused' : 'state-ready'"><i />{{ probe.status }}</span></span>
        <span class="probe-tab-meta"><span>{{ probe.channel }}</span><span>{{ probe.schedule }}</span><span>{{ t('qualityOps.probeAccountCount', { count: probe.count }) }}</span></span>
      </button>
      <button class="probe-configure" :disabled="disabled" :aria-label="t('qualityOps.configureProbeName', { name: probe.name })" :title="t('qualityOps.configureProbe')" :data-probe-configure="probe.id" @click="emit('configure', probe.id)"><Icon name="cog" size="sm" /></button>
    </div>
    <button v-if="standaloneCount || !probes.length" class="standalone-tab" role="tab" :aria-selected="standaloneSelected" aria-controls="quality-probe-panel" :tabindex="standaloneSelected || !probes.length ? 0 : -1" :disabled="disabled" data-probe-id="standalone" @click="emit('select', null)" @keydown="navigate($event, null)"><Icon name="users" size="sm" /><span>{{ t('qualityOps.standaloneRules') }}</span><span class="standalone-count">{{ standaloneCount }}</span></button>
  </div>
</template>
<script setup lang="ts">
import { nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
export interface ProbeTab { id: number; name: string; icon: 'sync' | 'bolt' | 'shield'; channel: string; schedule: string; count: number; status: string; warning: boolean; paused: boolean }
const props = defineProps<{ probes: ProbeTab[]; selectedId: number | null; standaloneSelected: boolean; standaloneCount: number; disabled?: boolean }>()
const emit = defineEmits<{ select: [id: number | null]; configure: [id: number] }>()
const { t } = useI18n()
async function navigate(event: KeyboardEvent, id: number | null) {
  if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) {
    if (id !== null) { event.preventDefault(); emit('configure', id) }
    return
  }
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const ids: (number | null)[] = props.probes.map(probe => probe.id)
  if (props.standaloneCount || !ids.length) ids.push(null)
  const index = ids.indexOf(id), delta = event.key === 'ArrowLeft' ? -1 : 1
  const next = event.key === 'Home' ? ids[0] : event.key === 'End' ? ids[ids.length - 1] : ids[(index + delta + ids.length) % ids.length]
  const list = (event.currentTarget as HTMLElement).closest('[role=tablist]')
  emit('select', next ?? null)
  await nextTick()
  list?.querySelector<HTMLButtonElement>(`[data-probe-id="${next ?? 'standalone'}"]`)?.focus()
}
</script>
<style scoped>
.probe-tabs { @apply flex gap-3 overflow-x-auto pb-1; scrollbar-width: thin; }
.probe-tab { @apply relative flex min-w-[250px] flex-1 rounded-xl border border-gray-200 bg-white transition-colors dark:border-dark-700 dark:bg-dark-900; }
.probe-tab.selected { @apply border-primary-500 bg-primary-50/60 ring-1 ring-primary-500 dark:bg-primary-950/30; }
.probe-tab-select { @apply min-w-0 flex-1 rounded-xl px-4 py-4 pr-11 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 focus-visible:ring-offset-2; }
.probe-tab-heading { @apply flex min-w-0 items-center gap-2 text-gray-700 dark:text-gray-200; }
.probe-tab-heading strong { @apply min-w-0 flex-1 truncate text-sm font-semibold; }
.selected .probe-tab-heading { @apply text-primary-800 dark:text-primary-200; }
.probe-tab-state { @apply inline-flex shrink-0 items-center gap-1.5 text-[11px]; }
.probe-tab-state i { @apply h-1.5 w-1.5 rounded-full bg-current; }
.state-ready { @apply text-emerald-600 dark:text-emerald-400; }
.state-warning { @apply text-amber-600 dark:text-amber-400; }
.state-paused { @apply text-gray-400; }
.probe-tab-meta { @apply mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-gray-500 dark:text-gray-400; }
.probe-configure { @apply absolute right-0 top-1 flex h-11 w-11 items-center justify-center rounded-lg text-gray-400 hover:bg-gray-100 hover:text-primary-600 focus-visible:ring-2 focus-visible:ring-primary-500 dark:hover:bg-dark-700; }
.standalone-tab { @apply flex min-w-[150px] shrink-0 items-center justify-center gap-2 rounded-xl border border-dashed border-gray-200 px-4 text-sm text-gray-500 dark:border-dark-600; }
.standalone-tab[aria-selected=true] { @apply border-solid border-primary-500 bg-primary-50 text-primary-700 dark:bg-primary-950/30 dark:text-primary-300; }
.standalone-count { @apply rounded bg-gray-100 px-1.5 text-xs tabular-nums dark:bg-dark-700; }
@media (max-width: 640px) { .probe-tab { min-width: 265px; } .probe-tab-select { padding-top: 12px; padding-bottom: 12px; } }
</style>
