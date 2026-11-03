<template>
  <div class="border-t border-gray-100 pt-4 dark:border-dark-700">
    <div class="flex items-center justify-between gap-4">
      <label for="account-rotation-toggle" class="font-medium text-gray-900 dark:text-white">
        {{ t('admin.settings.accountRotation.title') }}
      </label>
      <Toggle id="account-rotation-toggle" v-model="value.enabled" />
    </div>
    <template v-if="value.enabled">
      <div class="mt-4 rounded-xl bg-gray-50 p-4 dark:bg-dark-900/50">
        <div class="flex items-center justify-between gap-4">
          <label for="account-rotation-hold-toggle" class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.settings.accountRotation.scheduledHold') }}
          </label>
          <Toggle id="account-rotation-hold-toggle" v-model="value.scheduled_hold_enabled" />
        </div>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.accountRotation.scheduledHoldHint') }}</p>
      </div>
      <div class="mt-4 grid gap-4 sm:grid-cols-2" :class="{ 'lg:grid-cols-3': !value.scheduled_hold_enabled }">
        <label v-if="!value.scheduled_hold_enabled" class="block text-sm text-gray-700 dark:text-gray-300">
          {{ t('admin.settings.accountRotation.mode') }}
          <select v-model="value.mode" class="input mt-1 min-h-11 w-full">
            <option value="aggressive">{{ t('admin.settings.accountRotation.aggressive') }}</option>
            <option value="sticky">{{ t('admin.settings.accountRotation.sticky') }}</option>
          </select>
        </label>
        <label class="block text-sm text-gray-700 dark:text-gray-300">
          {{ t('admin.settings.accountRotation.work') }}
          <input v-model.number="value.work_minutes" type="number" min="1" max="1440" step="1" required class="input mt-1 min-h-11 w-full" />
        </label>
        <label class="block text-sm text-gray-700 dark:text-gray-300">
          {{ t('admin.settings.accountRotation.stagger') }}
          <input v-model.number="value.stagger_percent" type="number" min="0" max="30" step="1" required class="input mt-1 min-h-11 w-full" />
        </label>
        <label class="block text-sm text-gray-700 dark:text-gray-300">
          {{ t('admin.settings.accountRotation.restMin') }}
          <input v-model.number="value.rest_min_minutes" type="number" min="1" :max="value.rest_max_minutes" step="1" required class="input mt-1 min-h-11 w-full" />
        </label>
        <label class="block text-sm text-gray-700 dark:text-gray-300">
          {{ t('admin.settings.accountRotation.restMax') }}
          <input v-model.number="value.rest_max_minutes" type="number" :min="value.rest_min_minutes" max="120" step="1" required class="input mt-1 min-h-11 w-full" />
        </label>
      </div>
      <p class="mt-3 text-sm leading-relaxed text-gray-500 dark:text-gray-400">
        {{ t('admin.settings.accountRotation.timing') }}
        <template v-if="!value.scheduled_hold_enabled">
          {{ t(value.mode === 'aggressive' ? 'admin.settings.accountRotation.aggressiveHint' : 'admin.settings.accountRotation.stickyHint') }}
        </template>
      </p>
      <div class="mt-4 rounded-xl bg-gray-50 p-4 dark:bg-dark-900/50">
        <div class="flex items-center justify-between gap-4">
          <label for="account-rotation-early-toggle" class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.settings.accountRotation.earlyRest') }}
          </label>
          <Toggle id="account-rotation-early-toggle" v-model="value.early_rest_enabled" />
        </div>
        <div v-if="value.early_rest_enabled" class="mt-4 grid gap-4 sm:grid-cols-3">
          <label class="block text-sm text-gray-700 dark:text-gray-300">
            {{ t('admin.settings.accountRotation.earlyWindow') }}
            <input id="account-rotation-early-window" v-model.number="value.early_rest_window_minutes" type="number" min="1" max="1440" step="1" required class="input mt-1 min-h-11 w-full" />
          </label>
          <label class="block text-sm text-gray-700 dark:text-gray-300">
            {{ t('admin.settings.accountRotation.earlyConcurrency') }}
            <input id="account-rotation-early-concurrency" v-model.number="value.early_rest_concurrency_threshold" type="number" min="1" max="10000" step="1" required class="input mt-1 min-h-11 w-full" />
          </label>
          <label class="block text-sm text-gray-700 dark:text-gray-300">
            {{ t('admin.settings.accountRotation.earlySeconds') }}
            <input id="account-rotation-early-seconds" v-model.number="value.early_rest_seconds" type="number" min="1" max="7200" step="1" required class="input mt-1 min-h-11 w-full" />
          </label>
        </div>
        <p class="mt-2 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.accountRotation.earlyHint') }}</p>
      </div>
    </template>
    <div class="mt-4 grid gap-4 sm:grid-cols-2">
      <label class="block text-sm text-gray-700 dark:text-gray-300">
        {{ t('admin.settings.accountRotation.recoveryMin') }}
        <input id="account-rotation-recovery-min" v-model.number="recoveryMinSeconds" type="number" min="0.001" :max="recoveryMaxSeconds" step="0.001" required class="input mt-1 min-h-11 w-full" />
      </label>
      <label class="block text-sm text-gray-700 dark:text-gray-300">
        {{ t('admin.settings.accountRotation.recoveryMax') }}
        <input id="account-rotation-recovery-max" v-model.number="recoveryMaxSeconds" type="number" :min="recoveryMinSeconds" max="60" step="0.001" required class="input mt-1 min-h-11 w-full" />
      </label>
    </div>
    <p class="mt-3 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.settings.accountRotation.recoveryHint') }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import type { AccountRotationSettings } from '@/api/admin/settings'

const value = defineModel<AccountRotationSettings>({ required: true })
const { t } = useI18n()
const recoveryMinSeconds = computed({
  get: () => value.value.recovery_min_interval_ms / 1000,
  set: (seconds: number) => { value.value.recovery_min_interval_ms = Math.round(seconds * 1000) },
})
const recoveryMaxSeconds = computed({
  get: () => value.value.recovery_max_interval_ms / 1000,
  set: (seconds: number) => { value.value.recovery_max_interval_ms = Math.round(seconds * 1000) },
})
</script>
