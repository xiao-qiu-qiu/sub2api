<template>
  <details class="rounded-xl border border-gray-200 bg-gray-50/60 dark:border-dark-600 dark:bg-dark-800/50" data-testid="codex-fingerprint-advanced">
    <summary class="cursor-pointer rounded-xl px-4 py-3 text-sm font-medium text-gray-800 focus-visible:outline-primary-500 dark:text-gray-100">
      {{ label('title') }}
      <span class="ml-2 text-xs font-normal text-gray-500 dark:text-gray-400">{{ label('optional') }}</span>
    </summary>
    <div class="space-y-5 border-t border-gray-200 p-4 dark:border-dark-600">
      <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ label('intro') }}</p>

      <div class="space-y-2">
        <label :for="`${id}-scheme`" class="input-label">{{ label('scheme') }}</label>
        <Select :id="`${id}-scheme`" v-model="settings.scheme" :aria-label="label('scheme')" :options="schemeOptions" />
        <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ label('schemeHint') }}</p>
      </div>

      <section class="space-y-3">
        <h4 class="text-sm font-medium text-gray-800 dark:text-gray-100">{{ label('wireTitle') }}</h4>
        <div v-for="flag in codexWireFlags" :key="flag" class="flex items-start justify-between gap-4 border-b border-gray-200/70 pb-3 last:border-0 dark:border-dark-600">
          <div class="min-w-0">
            <p :id="`${id}-${flag}-label`" class="text-sm text-gray-700 dark:text-gray-200">{{ label(`flags.${flag}.title`) }}</p>
            <p :id="`${id}-${flag}-hint`" class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ label(`flags.${flag}.hint`) }}</p>
          </div>
          <Toggle v-model="settings.flags[flag]" :aria-labelledby="`${id}-${flag}-label`" :aria-describedby="`${id}-${flag}-hint`" :data-testid="`codex-wire-${flag}`" />
        </div>
      </section>

      <Input :id="`${id}-timezone`" v-model="settings.timezone" :label="label('timezone')" placeholder="Asia/Shanghai" :hint="label('timezoneHint')" />
      <fieldset class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
        <legend class="px-1 text-sm font-medium text-gray-800 dark:text-gray-100">{{ label('location') }}</legend>
        <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ label('locationHint') }}</p>
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <Input v-for="field in locationFields" :id="`${id}-location-${field}`" :key="field" v-model="settings.location[field]" :label="label(field)" :placeholder="field === 'timezone' ? 'Asia/Shanghai' : undefined" />
        </div>
      </fieldset>

      <section class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600">
        <label :for="`${id}-client`" class="input-label">{{ label('profile') }}</label>
        <Select :id="`${id}-client`" v-model="settings.client" :aria-label="label('profile')" :options="clientOptions" />
        <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ label('profileHint') }}</p>
        <div v-if="settings.client !== 'inherit'" class="space-y-4">
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div>
              <label :for="`${id}-platform`" class="input-label">{{ label('platform') }}</label>
              <Select :id="`${id}-platform`" v-model="settings.platform" :aria-label="label('platform')" :options="platformOptions" />
            </div>
            <div>
              <label :for="`${id}-version-mode`" class="input-label">{{ label('versionMode') }}</label>
              <Select :id="`${id}-version-mode`" v-model="settings.versionMode" :aria-label="label('versionMode')" :options="versionOptions" />
            </div>
          </div>
          <p class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ label('versionHint') }}</p>
          <Input v-if="settings.versionMode === 'fixed'" :id="`${id}-core`" v-model="settings.codexVersion" :label="label('coreVersion')" :hint="label('coreVersionRequired')" />
          <Input v-if="settings.client === 'desktop'" :id="`${id}-desktop`" v-model="settings.desktopVersion" :label="label('desktopVersion')" :hint="label('desktopVersionHint')" />
          <div v-if="settings.client === 'cli'" class="flex items-center justify-between gap-4">
            <span :id="`${id}-exec-label`" class="text-sm text-gray-700 dark:text-gray-200">{{ label('cliEntry') }}</span>
            <Toggle v-model="settings.cliEntry" :aria-labelledby="`${id}-exec-label`" />
          </div>
          <details class="rounded-lg border border-gray-200 p-3 dark:border-dark-600">
            <summary class="cursor-pointer text-sm text-gray-700 dark:text-gray-200">{{ label('overrides') }}</summary>
            <p class="mt-3 text-xs text-gray-500 dark:text-gray-400">{{ label('overridesHint') }}</p>
            <div class="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
              <Input v-for="field in profileFields" :id="`${id}-${field}`" :key="field" v-model="settings[field]" :label="label(field)" />
            </div>
          </details>
        </div>
      </section>
      <p v-if="validationError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ t(validationError) }}</p>
    </div>
  </details>
</template>

<script setup lang="ts">
import { computed, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import Input from '@/components/common/Input.vue'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import { codexWireFlags, validateCodexFingerprintSettings, type CodexFingerprintSettings } from '@/utils/codexFingerprintSettings'

const settings = defineModel<CodexFingerprintSettings>({ required: true })
const { t } = useI18n()
const id = useId()
const label = (key: string) => t(`admin.accounts.openai.fingerprintAdvanced.${key}`)
const schemeOptions = computed(() => [{ value: 'v1', label: label('v1') }, { value: 'v2', label: label('v2') }])
const clientOptions = computed(() => [
  { value: 'inherit', label: label('inherit') }, { value: 'cli', label: 'Codex CLI' }, { value: 'desktop', label: 'Codex Desktop' }
])
const platformOptions = [{ value: 'macos', label: 'macOS' }, { value: 'windows', label: 'Windows' }, { value: 'linux', label: 'Linux' }]
const versionOptions = computed(() => [{ value: 'latest', label: label('latest') }, { value: 'fixed', label: label('fixed') }])
const locationFields = ['city', 'region', 'country', 'timezone'] as const
const profileFields = ['osType', 'osVersion', 'arch', 'terminal'] as const
const validationError = computed(() => validateCodexFingerprintSettings(settings.value, undefined, true))
</script>
