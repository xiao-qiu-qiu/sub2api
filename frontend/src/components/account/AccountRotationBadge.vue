<template>
  <div class="flex flex-col items-start gap-1">
    <div class="flex flex-wrap items-center gap-1">
      <span data-testid="account-rotation-status" :class="['badge text-xs', resting || draining ? 'badge-warning' : 'badge-info']" :title="title">
        {{ t(`admin.settings.accountRotation.${label}`) }}
        <span v-if="resting" class="ml-1 font-mono tabular-nums">{{ countdown }}</span>
      </span>
      <span v-if="state.hold || (state.waiting_count ?? 0) > 0" data-testid="account-rotation-waiting" class="badge bg-violet-50 text-xs text-violet-700 dark:bg-violet-900/30 dark:text-violet-300" :title="t('admin.settings.accountRotation.holdHint')">
        {{ (state.waiting_count ?? 0) > 0 ? t('admin.settings.accountRotation.waitingCount', { count: state.waiting_count }) : t('admin.settings.accountRotation.holding') }}
      </span>
    </div>
    <div v-if="nextRest" data-testid="account-rotation-next" class="flex flex-wrap items-center gap-x-2 text-xs text-gray-600 dark:text-gray-300" :title="t('admin.settings.accountRotation.drainAt', { time: fullTime(state.drain_at) })">
      <span>{{ t('admin.settings.accountRotation.nextRest') }}</span>
      <span class="font-medium tabular-nums">{{ nextCountdown }}</span>
    </div>
    <div class="space-y-0.5 text-xs text-gray-500 dark:text-gray-400">
      <div data-testid="account-rotation-last-manual" :title="historyTitle(state.last_manual_rest_at)">
        {{ t('admin.settings.accountRotation.lastManual') }}
        <span class="ml-1 tabular-nums">{{ historyTime(state.last_manual_rest_at) }}</span>
      </div>
      <div data-testid="account-rotation-last-automatic" :title="historyTitle(state.last_automatic_rest_at)">
        {{ t(state.last_automatic_rest_reason === 'early' ? 'admin.settings.accountRotation.lastEarly' : 'admin.settings.accountRotation.lastAutomatic') }}
        <span class="ml-1 tabular-nums">{{ historyTime(state.last_automatic_rest_at) }}</span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useNow } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'

const props = defineProps<{ state: NonNullable<Account['account_rotation']> }>()
const { t } = useI18n()
const now = useNow({ interval: 1000 })
const resting = computed(() => props.state.phase === 'resting')
const draining = computed(() => props.state.phase === 'draining')
const watching = computed(() => props.state.phase === 'working' && (props.state.early_eligible_at ?? 0) > 0 && now.value.getTime() >= (props.state.early_eligible_at ?? 0) * 1000)
const label = computed(() => {
  if (props.state.recovering) return 'recovering'
  if (props.state.hold && (resting.value || draining.value)) {
    const reason = props.state.reason === 'early' ? 'early' : props.state.reason === 'manual' ? 'manual' : 'scheduled'
    return `${reason}${resting.value ? 'Resting' : 'Draining'}`
  }
  if (props.state.automatic_enabled === false) return 'disabled'
  return watching.value ? 'earlyWatching' : props.state.phase
})
const formatCountdown = (until: number) => {
  const seconds = Math.max(0, Math.ceil(until - now.value.getTime() / 1000))
  const minutes = Math.floor(seconds / 60)
  const tail = `${(minutes % 60).toString().padStart(2, '0')}:${(seconds % 60).toString().padStart(2, '0')}`
  return minutes >= 60 ? `${Math.floor(minutes / 60).toString().padStart(2, '0')}:${tail}` : tail
}
const countdown = computed(() => formatCountdown(props.state.rest_until))
const nextRest = computed(() => props.state.automatic_enabled !== false && props.state.phase === 'working' && props.state.drain_at > 0 && !props.state.recovering)
const nextCountdown = computed(() => formatCountdown(props.state.drain_at))
const fullTime = (at: number) => new Date(at * 1000).toLocaleString()
const historyTime = (at?: number) => at && at > 0
  ? new Date(at * 1000).toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
  : t('admin.settings.accountRotation.noHistory')
const historyTitle = (at?: number) => at && at > 0 ? t('admin.settings.accountRotation.restStartedAt', { time: fullTime(at) }) : undefined
const title = computed(() => {
  if (props.state.recovering) return t('admin.settings.accountRotation.recoveryHint')
  if (resting.value) return t('admin.settings.accountRotation.restUntil', { time: new Date(props.state.rest_until * 1000).toLocaleString() })
  if (draining.value && props.state.hold) return t('admin.settings.accountRotation.drainHoldHint', { seconds: props.state.rest_seconds })
  if (props.state.drain_at > 0) return t('admin.settings.accountRotation.drainAt', { time: new Date(props.state.drain_at * 1000).toLocaleString() })
  return undefined
})
</script>
