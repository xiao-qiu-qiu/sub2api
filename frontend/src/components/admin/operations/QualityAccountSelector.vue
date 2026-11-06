<template>
  <div class="space-y-3">
    <label for="quality-account-search" class="block text-sm font-medium">{{ t('qualityOps.accounts') }}</label>
    <div class="grid gap-3 sm:grid-cols-2">
      <label class="space-y-1 text-sm"><span>{{ t('qualityOps.accountGroup') }}</span>
        <select v-model="accountGroup" class="input" data-testid="quality-account-group" @change="emit('search', 1)">
          <option value="">{{ t('qualityOps.allAccountGroups') }}</option>
          <option value="ungrouped">{{ t('qualityOps.ungroupedAccounts') }}</option>
          <option v-for="group in groups" :key="group.id" :value="String(group.id)">{{ group.name }} #{{ group.id }}</option>
        </select>
      </label>
      <label class="space-y-1 text-sm"><span>{{ t('qualityOps.accountType') }}</span>
        <select v-model="accountType" class="input" data-testid="quality-account-type" @change="emit('search', 1)">
          <option value="">{{ t('qualityOps.allAccountTypes') }}</option>
          <option v-for="option in qualityAccountTypeOptions" :key="option.value" :value="option.value">{{ t(option.label) }}</option>
        </select>
      </label>
    </div>
    <div class="space-y-1 text-sm">
      <span>{{ t('qualityOps.accountStatus') }}</span>
      <div class="flex flex-wrap gap-x-4 gap-y-1">
        <label v-for="option in qualityAccountStatusOptions" :key="option.value" class="flex items-center gap-1.5">
          <input v-model="statuses" type="checkbox" :value="option.value" :data-testid="`quality-account-status-${option.value}`" @change="emit('search', 1)" />{{ t(option.label) }}
        </label>
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('qualityOps.accountStatusHint') }}</p>
    </div>
    <div class="flex gap-2"><input id="quality-account-search" v-model="search" class="input min-w-0" :placeholder="t('qualityOps.search')" @keydown.enter.prevent="emit('search', 1)" /><button type="button" class="btn btn-secondary shrink-0 whitespace-nowrap" @click="emit('search', 1)">{{ t('qualityOps.search') }}</button></div>
    <div v-if="!bulk && !scopeLocked && !hideScope" class="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
      <span>{{ t('qualityOps.accountScope') }}</span>
      <label class="flex items-center gap-1.5"><input v-model="scope" type="radio" value="all" data-testid="quality-account-scope-all" />{{ t('qualityOps.accountScopeAll') }}</label>
      <label class="flex items-center gap-1.5"><input v-model="scope" type="radio" value="manual" data-testid="quality-account-scope-manual" />{{ t('qualityOps.accountScopeManual') }}</label>
    </div>
    <div v-if="!allScope" class="flex flex-wrap items-center gap-3 text-sm">
      <button type="button" class="text-primary-600 disabled:opacity-50" data-testid="quality-select-page" :disabled="accountsLoading || selectingAccounts || !accounts.some(account => !disabledReason(account))" @click="emit('selectPage')">{{ t('qualityOps.selectAccountPage') }}</button>
      <button type="button" class="text-primary-600 disabled:opacity-50" data-testid="quality-select-all" :disabled="accountsLoading || selectingAccounts || !accounts.length" @click="emit('selectAll')">{{ t(selectingAccounts ? 'qualityOps.selectingAccounts' : 'qualityOps.selectMatchingAccounts') }}</button>
      <button type="button" class="text-gray-500 disabled:opacity-50" data-testid="quality-clear-selection" :disabled="!selectedAccounts.length && !selectingAccounts" @click="emit('clear')">{{ t('qualityOps.clearAccountSelection') }}</button>
    </div>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ selectionHint || t(scopeLocked ? 'qualityOps.templateFilterHint' : allScope ? 'qualityOps.accountScopeAllHint' : bulk ? 'qualityOps.bulkAccountSelectionHint' : 'qualityOps.accountSelectionHint') }}</p>
    <p v-if="accountsError" role="alert" class="text-sm text-red-600">{{ accountsError }}</p>
    <p v-if="accountsLoading" role="status" class="text-sm text-gray-500">{{ t('qualityOps.loading') }}</p>
    <div class="grid max-h-52 gap-2 overflow-auto rounded border p-3 sm:grid-cols-2 dark:border-dark-600" :aria-busy="accountsLoading || selectingAccounts">
      <div v-for="account in accounts" :key="account.id" :data-testid="`quality-account-${account.id}`" class="flex items-center gap-2 rounded-md p-1 text-sm">
        <input v-if="!allScope" :id="`quality-select-account-${account.id}`" v-model="selectedAccounts" type="checkbox" :value="account.id" :disabled="disabled || selectingAccounts || !!disabledReason(account)" />
        <div class="min-w-0 flex-1">
          <component :is="allScope ? 'span' : 'label'" :for="allScope ? undefined : `quality-select-account-${account.id}`" class="break-all">{{ account.name }} <span class="text-gray-500 dark:text-gray-400">#{{ account.id }}</span></component>
          <span v-if="scopeLocked && excludedAccountIds?.includes(account.id)" class="ml-1 inline-block rounded px-1.5 py-0.5 text-xs" :class="reincludeAccountIds?.includes(account.id) ? 'bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300' : 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'">{{ t(reincludeAccountIds?.includes(account.id) ? 'qualityOps.accountReincludePending' : 'qualityOps.accountExcluded') }}</span>
          <span v-if="disabledReason(account)" class="ml-1 text-xs text-gray-500 dark:text-gray-400">{{ disabledReason(account) }}</span>
        </div>
        <button v-if="scopeLocked && excludedAccountIds?.includes(account.id)" type="button" :data-testid="`quality-reinclude-${account.id}`" class="min-h-11 shrink-0 rounded-md px-2 text-xs font-medium text-primary-600 transition-colors hover:bg-primary-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-500 disabled:cursor-not-allowed disabled:opacity-50 dark:text-primary-400 dark:hover:bg-primary-900/30" :disabled="disabled || accountsLoading || selectingAccounts || (!reincludeAccountIds?.includes(account.id) && !!disabledReason(account))" @click="emit('toggleReinclude', account.id)">{{ t(reincludeAccountIds?.includes(account.id) ? 'qualityOps.undoReinclude' : 'qualityOps.reincludeAccount') }}</button>
      </div>
      <p v-if="!accountsLoading && !accounts.length" class="text-sm text-gray-500 sm:col-span-2">{{ t('qualityOps.noMatchingAccounts') }}</p>
    </div>
    <div class="flex flex-wrap items-center gap-3 text-sm"><button type="button" :aria-label="t('qualityOps.previousAccountPage')" :disabled="accountsLoading || selectingAccounts || accountPage <= 1" @click="emit('search', accountPage - 1)">←</button><span>{{ accountPage }} / {{ accountPages }}</span><button type="button" :aria-label="t('qualityOps.nextAccountPage')" :disabled="accountsLoading || selectingAccounts || accountPage >= accountPages" @click="emit('search', accountPage + 1)">→</button><span aria-live="polite" data-testid="quality-account-count">{{ allScope ? t('qualityOps.accountsMatched', { count: accountsTotal }) : t('qualityOps.selected', { count: selectedAccounts.length }) }}</span></div>
    <p v-if="scopeLocked && reincludeAccountIds?.length" class="text-xs text-primary-700 dark:text-primary-300" aria-live="polite" data-testid="quality-reinclude-count">{{ t('qualityOps.reincludePendingCount', { count: reincludeAccountIds.length }) }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountListItem, AdminGroup } from '@/types'
import { qualityAccountStatusOptions, qualityAccountTypeOptions } from '@/utils/qualityAccountFilter'

const { t } = useI18n()
const selectedAccounts = defineModel<number[]>({ required: true })
const search = defineModel<string>('search', { required: true })
const accountGroup = defineModel<string>('group', { required: true })
const accountType = defineModel<string>('type', { required: true })
// 状态可多选（任一命中），全不勾 = 全部状态；范围默认「所有匹配账户」，保存时按筛选自动包含。
const statuses = defineModel<string[]>('statuses', { required: true })
const scope = defineModel<'all' | 'manual'>('scope', { required: true })
const props = defineProps<{
  accounts: AccountListItem[]
  groups: AdminGroup[]
  accountsLoading: boolean
  selectingAccounts: boolean
  accountsError: string
  accountPage: number
  accountPages: number
  accountsTotal: number
  bulk: boolean
  // 编辑分组规则时列表展示覆盖和排除状态，重新纳入作为独立操作随表单保存。
  scopeLocked?: boolean
  excludedAccountIds?: number[]
  reincludeAccountIds?: number[]
  disabled?: boolean
  hideScope?: boolean
  selectionHint?: string
  disabledReason: (account: AccountListItem) => string
}>()
// 批量修改改的是已有规则，范围选择只在新建时出现。
const allScope = computed(() => scope.value === 'all' && !props.bulk)
const emit = defineEmits<{
  search: [page: number]
  selectPage: []
  selectAll: []
  clear: []
  toggleReinclude: [accountId: number]
}>()
</script>
