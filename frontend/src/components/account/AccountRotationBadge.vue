<template>
  <div class="flex flex-wrap items-center gap-1">
    <span data-testid="account-rotation-status" :class="['badge text-xs', resting || draining ? 'badge-warning' : 'badge-info']" :title="title">
      {{ t(`admin.settings.accountRotation.${label}`) }}
      <span v-if="resting" class="ml-1 font-mono tabular-nums">{{ countdown }}</span>
    </span>
    <span v-if="state.hold || (state.waiting_count ?? 0) > 0" data-testid="account-rotation-waiting" class="badge bg-violet-50 text-xs text-violet-700 dark:bg-violet-900/30 dark:text-violet-300" :title="t('admin.settings.accountRotation.holdHint')">
      {{ (state.waiting_count ?? 0) > 0 ? t('admin.settings.accountRotation.waitingCount', { count: state.waiting_count }) : t('admin.settings.accountRotation.holding') }}
    </span>
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
    return `${props.state.reason === 'early' ? 'early' : 'manual'}${resting.value ? 'Resting' : 'Draining'}`
  }
  return watching.value ? 'earlyWatching' : props.state.phase
})
const countdown = computed(() => {
  const seconds = Math.max(0, Math.ceil(props.state.rest_until - now.value.getTime() / 1000))
  return `${Math.floor(seconds / 60).toString().padStart(2, '0')}:${(seconds % 60).toString().padStart(2, '0')}`
})
const title = computed(() => {
  if (props.state.recovering) return t('admin.settings.accountRotation.recoveryHint')
  if (resting.value) return t('admin.settings.accountRotation.restUntil', { time: new Date(props.state.rest_until * 1000).toLocaleString() })
  if (draining.value && props.state.hold) return t('admin.settings.accountRotation.drainHoldHint', { minutes: props.state.rest_seconds / 60 })
  if (props.state.drain_at > 0) return t('admin.settings.accountRotation.drainAt', { time: new Date(props.state.drain_at * 1000).toLocaleString() })
  return undefined
})
</script>
