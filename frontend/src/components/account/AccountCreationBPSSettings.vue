<template>
  <section class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-600" data-testid="create-bps-settings">
    <label class="flex items-center gap-2 text-sm font-medium">
      <input :checked="forced || draft.enabled" type="checkbox" :disabled="forced || (!available && !draft.enabled)" data-testid="create-bps-enable" @change="draft.enabled = ($event.target as HTMLInputElement).checked" />
      {{ t('admin.accounts.creationPreset.enableBPS') }}
      <span class="cursor-help text-gray-400" :title="t('admin.accounts.creationPreset.bpsAuthorizationHint')">ⓘ</span>
    </label>
    <p v-if="!available" class="text-xs text-amber-600" role="status">{{ t('admin.accounts.creationPreset.bpsUnavailable') }}</p>
    <div v-if="available && (forced || draft.enabled)" class="space-y-3">
      <ExcelBPSTransportSelect v-model="draft.transport" prefix="bps-oauth" />
      <label class="flex items-center gap-2 text-sm"><input v-model="draft.all_models" type="checkbox" data-testid="create-bps-all-models" />{{ t('admin.accounts.openai.excelBPSAllModels') }}</label>
      <div v-if="!draft.all_models" data-testid="bps-oauth-models">
        <label class="input-label">{{ t('admin.accounts.openai.excelBPSModels') }}</label>
        <ModelWhitelistSelector v-model="draft.models" platform="openai" />
      </div>
      <details class="rounded-lg border border-gray-200 dark:border-dark-600">
        <summary class="cursor-pointer px-3 py-2 text-sm">{{ t('autoConfig.bps.advanced') }}</summary>
        <div class="grid gap-3 border-t border-gray-200 p-3 sm:grid-cols-2 dark:border-dark-600">
          <div v-for="item in options" :key="item.key" class="space-y-2">
            <label class="flex items-center gap-2 text-sm" :title="t('autoConfig.bps.' + item.key + 'Hint')"><input v-model="draft[item.key]" type="checkbox" :disabled="item.key === 'auto_recover_on_403' && !draft.auto_disable_on_403" :data-testid="'create-bps-' + item.key" />{{ t('admin.accounts.openai.excelBPS' + item.label) }}</label>
            <label v-if="item.key === 'auto_recover_on_403' && draft.auto_recover_on_403 && draft.auto_disable_on_403" class="block text-xs">{{ t('admin.accounts.openai.excelBPS403RecoveryInterval') }}<input v-model.number="draft.recovery_interval_minutes" type="number" min="1" :max="MAX_BPS_RECOVERY_INTERVAL_MINUTES" class="input mt-1" required /></label>
            <select v-if="item.key === 'auto_move_on_403' && draft.auto_move_on_403" v-model.number="draft.target_group_id" class="input" :aria-label="t('admin.accounts.openai.excelBPS403TargetGroup')"><option :value="-1" disabled>{{ t('admin.accounts.openai.excelBPS403SelectTarget') }}</option><option :value="0">{{ t('admin.accounts.openai.excelBPS403LeaveAllGroups') }}</option><option v-for="group in targetGroups" :key="group.id" :value="group.id">{{ group.name }}</option></select>
            <select v-if="item.key === 'session_proxy' && draft.session_proxy" v-model="draft.proxy_source" class="input" :aria-label="t('admin.accounts.openai.excelBPSProxySource')"><option value="mihomo">{{ t('admin.accounts.openai.excelBPSProxySourceMihomo') }}</option><option value="ip_pool">{{ t('admin.accounts.openai.excelBPSProxySourceIPPool') }}</option></select>
          </div>
        </div>
      </details>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AdminGroup } from '@/types'
import type { AccountCreationBPS } from '@/utils/accountCreationBPS'
import { MAX_BPS_RECOVERY_INTERVAL_MINUTES } from '@/utils/excelBPSRecovery'
import ExcelBPSTransportSelect from './ExcelBPSTransportSelect.vue'
import ModelWhitelistSelector from './ModelWhitelistSelector.vue'
const props = defineProps<{ groups: AdminGroup[]; forced: boolean; available: boolean }>()
const draft = defineModel<AccountCreationBPS>({ required: true })
const { t } = useI18n()
const targetGroups = computed(() => props.groups.filter(group => group.platform === 'openai' || group.platform === 'composite'))
const options = [
  { key: 'ignore_encrypted_content', label: 'IgnoreEncryptedContent' },
  { key: 'omit_unsupported_tools', label: 'OmitUnsupportedTools' },
  { key: 'auto_disable_on_403', label: 'AutoDisableOn403' },
  { key: 'auto_recover_on_403', label: 'AutoRecoverOn403' },
  { key: 'auto_move_on_403', label: 'AutoMoveOn403' },
  { key: 'session_proxy', label: 'Mihomo' },
  { key: 'cache_creation_as_input', label: 'CacheCreationAsInput' }
] as const
watch(() => draft.value.auto_disable_on_403, enabled => { if (!enabled) draft.value.auto_recover_on_403 = false })
</script>
