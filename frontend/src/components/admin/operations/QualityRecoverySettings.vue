<template>
  <section v-if="standalone || (supported && policy.auto_restore)" class="recovery-settings" data-testid="quality-recovery-settings">
    <div v-if="standalone" class="flex items-center justify-between gap-3"><strong>{{ t('qualityOps.recoveryProbe') }}</strong><button type="button" class="text-xs text-primary-600 dark:text-primary-400" data-testid="quality-recovery-preset" @click="applyPreset">{{ t('qualityOps.recoveryQuickSetup') }}</button></div>
    <label v-else class="flex items-center gap-2 font-medium"><input type="checkbox" :checked="policy.recovery_probe_enabled" data-testid="quality-recovery-enabled" @change="set('recovery_probe_enabled', ($event.target as HTMLInputElement).checked)" />{{ t('qualityOps.legacyRecoveryProbe') }}</label>
    <div v-if="standalone || policy.recovery_probe_enabled" class="mt-4 space-y-3">
      <div class="grid gap-4 sm:grid-cols-2">
        <label v-if="editInterval" class="space-y-1"><span>{{ t('qualityOps.recoveryInterval') }}</span><div class="flex items-center gap-2"><input class="input" type="number" min="1" max="10080" required :value="policy.recovery_probe_interval_minutes ?? 60" data-testid="quality-recovery-interval" @input="set('recovery_probe_interval_minutes', Number(($event.target as HTMLInputElement).value))" /><span>{{ t('qualityOps.minutesUnit') }}</span></div></label>
        <label v-if="editConfirmation" class="space-y-1"><span>{{ t('qualityOps.recoveryConfirmation') }}</span><div class="flex items-center gap-2"><input class="input" type="number" min="1" max="10080" required :value="policy.recovery_confirmation_minutes ?? 5" data-testid="quality-recovery-confirmation" @input="set('recovery_confirmation_minutes', Number(($event.target as HTMLInputElement).value))" /><span>{{ t('qualityOps.minutesUnit') }}</span></div></label>
      </div>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { QualityPolicy } from '@/types'
const props = withDefaults(defineProps<{ policy: QualityPolicy; editInterval?: boolean; editConfirmation?: boolean }>(), { editInterval: true, editConfirmation: true })
const emit = defineEmits<{ 'update:policy': [policy: QualityPolicy] }>()
const { t } = useI18n()
const supported = computed(() => ['move_groups', 'remove_groups', 'disable_scheduling'].includes(props.policy.action))
const standalone = computed(() => props.policy.action_on === 'passed')
function applyPreset() {
  emit('update:policy', { ...props.policy, ...(props.editInterval ? { recovery_probe_interval_minutes: 60 } : {}), ...(props.editConfirmation ? { recovery_confirmation_minutes: 5 } : {}) })
}
function set(key: keyof QualityPolicy, value: boolean | number) {
  emit('update:policy', { recovery_probe_interval_minutes: 60, recovery_confirmation_minutes: 5, ...props.policy, [key]: value })
}
</script>
<style scoped>
.recovery-settings { @apply rounded-xl border border-primary-200 bg-primary-50/50 p-4 text-sm dark:border-primary-900/60 dark:bg-primary-950/20; }
.recovery-settings input[type='number'] { min-width: 0; }
.recovery-settings input + span { flex-shrink: 0; white-space: nowrap; }
</style>
