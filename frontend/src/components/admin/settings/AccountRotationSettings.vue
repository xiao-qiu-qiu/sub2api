<template>
  <div class="border-t border-gray-100 pt-4 dark:border-dark-700">
    <div class="flex items-center justify-between gap-4">
      <label for="account-rotation-toggle" class="font-medium text-gray-900 dark:text-white">
        {{ t('admin.settings.accountRotation.title') }}
      </label>
      <Toggle id="account-rotation-toggle" v-model="value.enabled" />
    </div>
    <template v-if="value.enabled">
      <div class="mt-4 grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <label class="block text-sm text-gray-700 dark:text-gray-300">
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
        {{ t(value.mode === 'aggressive' ? 'admin.settings.accountRotation.aggressiveHint' : 'admin.settings.accountRotation.stickyHint') }}
      </p>
    </template>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import type { AccountRotationSettings } from '@/api/admin/settings'

const value = defineModel<AccountRotationSettings>({ required: true })
const { t } = useI18n()
</script>
