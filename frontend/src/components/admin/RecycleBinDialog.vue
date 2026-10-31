<template>
  <BaseDialog
    :show="show"
    :title="t(kind === 'accounts' ? 'trash.accountTitle' : 'trash.proxyTitle')"
    width="extra-wide"
    :show-close-button="busyId === null"
    :close-on-escape="busyId === null && !pendingDelete"
    @close="close"
  >
    <div class="space-y-5">
      <p class="text-sm leading-6 text-gray-500 dark:text-gray-400">
        {{ t(kind === 'accounts' ? 'trash.accountDescription' : 'trash.proxyDescription') }}
      </p>
      <div class="flex items-center gap-3">
        <div class="relative flex-1 sm:max-w-sm">
          <Icon name="search" size="md" class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" />
          <input
            v-model="search"
            type="search"
            class="input pl-10"
            :placeholder="t('trash.searchPlaceholder')"
            :aria-label="t('trash.searchPlaceholder')"
            :disabled="busyId !== null"
            @input="scheduleSearch"
          />
        </div>
        <button type="button" class="btn btn-secondary" :disabled="loading || busyId !== null" :title="t('common.refresh')" :aria-label="t('common.refresh')" @click="load">
          <Icon name="refresh" size="md" :class="{ 'animate-spin': loading }" />
        </button>
      </div>

      <div v-if="loadError" role="alert" class="flex flex-wrap items-center justify-between gap-3 rounded-xl bg-red-50 p-4 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">
        <span>{{ loadError }}</span>
        <button type="button" class="btn btn-secondary btn-sm" @click="load">{{ t('trash.retry') }}</button>
      </div>
      <DataTable v-else :columns="columns" :data="entries" :loading="loading">
        <template #cell-name="{ row }">
          <div class="font-medium text-gray-900 dark:text-white">{{ row.name }}</div>
          <div class="mt-1 text-xs text-gray-400">#{{ row.id }}</div>
        </template>
        <template #cell-details="{ row }">
          <div v-if="kind === 'proxies'" class="break-all font-mono text-xs text-gray-600 dark:text-gray-300">{{ proxyAddress(row) }}</div>
          <div v-else class="space-y-1">
            <span class="badge badge-gray">{{ row.platform }} · {{ row.type }}</span>
            <div v-if="row.login_email" class="break-all text-xs text-gray-500 dark:text-gray-400">{{ row.login_email }}</div>
          </div>
        </template>
        <template #cell-login_credentials="{ row }">
          <span :class="row.has_login_credentials ? 'badge badge-success' : 'text-xs text-gray-400'">
            {{ t(row.has_login_credentials ? 'trash.credentialsArchived' : 'trash.noCredentials') }}
          </span>
        </template>
        <template #cell-deleted_at="{ value }">
          <span class="whitespace-nowrap text-sm text-gray-500 dark:text-gray-400">{{ formatDateTime(value) }}</span>
        </template>
        <template #cell-actions="{ row }">
          <div class="flex flex-wrap items-center justify-end gap-2">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              :disabled="busyId !== null || loading"
              :aria-label="`${t('trash.restore')} ${row.name}`"
              @click.stop="restore(row)"
            >
              <Icon name="refresh" size="sm" class="mr-1.5" :class="{ 'animate-spin': busyId === row.id && busyAction === 'restore' }" />
              {{ t(busyId === row.id && busyAction === 'restore' ? 'trash.restoring' : 'trash.restore') }}
            </button>
            <button
              type="button"
              class="btn btn-danger btn-sm"
              :disabled="busyId !== null || loading"
              :aria-label="`${t('trash.permanentlyDelete')} ${row.name}`"
              @click.stop="pendingDelete = row"
            >
              {{ t('trash.permanentlyDelete') }}
            </button>
          </div>
        </template>
        <template #empty>
          <div class="py-12 text-center">
            <div class="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-gray-100 text-gray-400 dark:bg-dark-700">
              <Icon name="trash" size="lg" />
            </div>
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ t(search.trim() ? 'trash.noResults' : 'trash.empty') }}</p>
          </div>
        </template>
      </DataTable>
      <div v-if="total > 0 && !loadError" :class="{ 'pointer-events-none opacity-50': busyId !== null }" :inert="busyId !== null || undefined">
        <Pagination :page="page" :page-size="pageSize" :total="total" @update:page="changePage" @update:pageSize="changePageSize" />
      </div>
    </div>
  </BaseDialog>

  <BaseDialog
    :show="show && pendingDelete !== null"
    :title="t('trash.permanentDeleteTitle')"
    width="narrow"
    :z-index="60"
    :show-close-button="busyId === null"
    :close-on-escape="busyId === null"
    @close="cancelDelete"
  >
    <p class="text-sm leading-6 text-gray-600 dark:text-gray-300">
      {{ t('trash.permanentDeleteConfirm', { name: pendingDelete?.name }) }}
    </p>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="busyId !== null" @click="cancelDelete">{{ t('common.cancel') }}</button>
        <button type="button" class="btn btn-danger" :disabled="busyId !== null" @click="permanentlyDelete">
          {{ t(busyAction === 'delete' ? 'trash.permanentlyDeleting' : 'trash.permanentlyDelete') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { listTrash, permanentlyDeleteTrash, restoreTrash, type TrashEntry, type TrashKind } from '@/api/admin/trash'
import BaseDialog from '@/components/common/BaseDialog.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import type { Column } from '@/components/common/types'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores/app'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{ show: boolean; kind: TrashKind }>()
const emit = defineEmits<{ close: []; changed: [] }>()
const { t } = useI18n()
const appStore = useAppStore()
const entries = ref<TrashEntry[]>([])
const search = ref('')
const page = ref(1)
const pageSize = ref(20)
const total = ref(0)
const loading = ref(false)
const loadError = ref('')
const busyId = ref<number | null>(null)
const busyAction = ref<'restore' | 'delete' | null>(null)
const pendingDelete = ref<TrashEntry | null>(null)
let searchTimer: ReturnType<typeof setTimeout> | undefined
let loadController: AbortController | undefined
let loadVersion = 0

const columns = computed<Column[]>(() => [
  { key: 'name', label: t('trash.name') },
  { key: 'details', label: t('trash.details') },
  ...(props.kind === 'accounts' ? [{ key: 'login_credentials', label: t('trash.loginCredentials') }] : []),
  { key: 'deleted_at', label: t('trash.deletedAt') },
  { key: 'actions', label: t('common.actions'), class: 'text-right' }
])

function proxyAddress(entry: TrashEntry): string {
  const host = entry.host?.includes(':') && !entry.host.startsWith('[') ? `[${entry.host}]` : entry.host
  return `${entry.protocol}://${host}:${entry.port}`
}

async function load() {
  if (!props.show) return
  clearTimeout(searchTimer)
  loadController?.abort()
  const controller = new AbortController()
  loadController = controller
  const version = ++loadVersion
  loading.value = true
  loadError.value = ''
  try {
    const result = await listTrash(props.kind, page.value, pageSize.value, search.value.trim(), controller.signal)
    if (version !== loadVersion || controller.signal.aborted) return
    total.value = result.total
    const lastPage = Math.max(1, Math.ceil(result.total / pageSize.value))
    if (page.value > lastPage) {
      page.value = lastPage
      await load()
      return
    }
    entries.value = result.items
  } catch (error) {
    if (version === loadVersion && !controller.signal.aborted) {
      loadError.value = extractI18nErrorMessage(error, t, 'trash.errors', t('trash.loadFailed'))
    }
  } finally {
    if (version === loadVersion) loading.value = false
  }
}

function scheduleSearch() {
  clearTimeout(searchTimer)
  // Invalidate the previous query immediately, before the debounce finishes.
  loadController?.abort()
  searchTimer = setTimeout(() => {
    page.value = 1
    void load()
  }, 300)
}

function changePage(value: number) {
  if (busyId.value !== null) return
  page.value = value
  void load()
}

function changePageSize(value: number) {
  if (busyId.value !== null) return
  pageSize.value = value
  page.value = 1
  void load()
}

async function restore(entry: TrashEntry) {
  if (busyId.value !== null) return
  busyId.value = entry.id
  busyAction.value = 'restore'
  try {
    await restoreTrash(props.kind, entry.id)
    appStore.showSuccess(t('trash.restoreSuccess'))
    emit('changed')
    await load()
  } catch (error) {
    appStore.showError(extractI18nErrorMessage(error, t, 'trash.errors', t('trash.restoreFailed')))
  } finally {
    busyId.value = null
    busyAction.value = null
  }
}

async function permanentlyDelete() {
  if (!pendingDelete.value || busyId.value !== null) return
  busyId.value = pendingDelete.value.id
  busyAction.value = 'delete'
  try {
    await permanentlyDeleteTrash(props.kind, pendingDelete.value.id)
    pendingDelete.value = null
    appStore.showSuccess(t('trash.permanentDeleteSuccess'))
    emit('changed')
    await load()
  } catch (error) {
    appStore.showError(extractI18nErrorMessage(error, t, 'trash.errors', t('trash.permanentDeleteFailed')))
  } finally {
    busyId.value = null
    busyAction.value = null
  }
}

function close() {
  if (busyId.value === null && !pendingDelete.value) emit('close')
}

function cancelDelete() {
  if (busyId.value === null) pendingDelete.value = null
}

watch(() => [props.show, props.kind] as const, ([show]) => {
  clearTimeout(searchTimer)
  loadController?.abort()
  loadVersion++
  pendingDelete.value = null
  if (show) {
    search.value = ''
    page.value = 1
    total.value = 0
    entries.value = []
    void load()
  }
}, { immediate: true })

onBeforeUnmount(() => {
  clearTimeout(searchTimer)
  loadController?.abort()
  loadVersion++
})
</script>
