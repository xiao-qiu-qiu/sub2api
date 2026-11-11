<template>
  <div>
    <label :for="prefix + '-transport'" class="input-label">{{ t('admin.accounts.openai.excelBPSTransport') }}</label>
    <select :id="prefix + '-transport'" v-model="transport" :data-testid="prefix + '-transport'" class="input">
      <option value="sse">{{ t('admin.accounts.openai.excelBPSTransportSSE') }}</option>
      <option value="ws">{{ t('admin.accounts.openai.excelBPSTransportWS') }}</option>
    </select>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { ExcelBPSTransport } from '@/utils/excelBPSTransport'

withDefaults(defineProps<{ prefix?: string }>(), { prefix: 'excel-bps' })
const transport = defineModel<ExcelBPSTransport>({ default: 'sse' })
const { t } = useI18n()
</script>
