<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{
  modelValue: string
  defaultInstructions: string
}>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
const { t } = useI18n()
const expanded = ref(false)
const characterCount = computed(() => Array.from(props.modelValue.trim()).length)
const isDefault = computed(() => !props.modelValue.trim() || props.modelValue.trim() === props.defaultInstructions.trim())
const invalid = computed(() => characterCount.value > 16000)

function updateInstructions(event: Event) {
  emit('update:modelValue', (event.target as HTMLTextAreaElement).value)
}
</script>

<template>
  <section class="card" data-testid="excel-bps-client-instructions-settings">
    <div class="flex flex-col gap-4 px-6 py-4 sm:flex-row sm:items-center sm:justify-between">
      <div class="min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <h2 id="settings-section-features-excel-bps-client-instructions" tabindex="-1" class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('admin.settings.features.excelBpsInstructions.title') }}
          </h2>
          <span class="rounded-full bg-gray-100 px-2 py-0.5 text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300" data-testid="excel-bps-instructions-mode">
            {{ t(isDefault ? 'admin.settings.features.excelBpsInstructions.default' : 'admin.settings.features.excelBpsInstructions.custom') }}
          </span>
        </div>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.settings.features.excelBpsInstructions.description') }}
        </p>
      </div>
      <button
        type="button"
        class="btn btn-secondary min-h-11 shrink-0"
        aria-controls="excel-bps-client-instructions-editor"
        :aria-expanded="expanded"
        data-testid="excel-bps-instructions-toggle"
        @click="expanded = !expanded"
      >
        {{ t(expanded ? 'admin.settings.features.excelBpsInstructions.collapse' : 'admin.settings.features.excelBpsInstructions.edit') }}
      </button>
    </div>
    <div v-show="expanded" id="excel-bps-client-instructions-editor" class="space-y-3 border-t border-gray-100 p-6 dark:border-dark-700">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <label for="excel-bps-client-instructions" class="input-label mb-0">
          {{ t('admin.settings.features.excelBpsInstructions.label') }}
        </label>
        <button
          type="button"
          class="btn btn-secondary min-h-11 text-sm"
          :disabled="modelValue === defaultInstructions || !defaultInstructions"
          data-testid="excel-bps-instructions-reset"
          @click="emit('update:modelValue', defaultInstructions)"
        >
          {{ t('admin.settings.features.excelBpsInstructions.reset') }}
        </button>
      </div>
      <textarea
        id="excel-bps-client-instructions"
        :value="modelValue"
        :placeholder="defaultInstructions"
        :aria-invalid="invalid"
        aria-describedby="excel-bps-client-instructions-hint"
        class="input min-h-64 resize-y font-mono text-sm leading-6"
        rows="13"
        spellcheck="false"
        @input="updateInstructions"
      />
      <div id="excel-bps-client-instructions-hint" class="flex flex-wrap items-start justify-between gap-2 text-xs">
        <span :class="invalid ? 'text-red-600 dark:text-red-400' : 'text-gray-500 dark:text-gray-400'">
          {{ t(invalid ? 'admin.settings.features.excelBpsInstructions.tooLong' : 'admin.settings.features.excelBpsInstructions.hint') }}
        </span>
        <span class="shrink-0 tabular-nums text-gray-500 dark:text-gray-400">{{ characterCount.toLocaleString() }} / 16,000</span>
      </div>
    </div>
  </section>
</template>
