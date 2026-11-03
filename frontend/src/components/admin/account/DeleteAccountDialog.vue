<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.deleteAccount')"
    width="narrow"
    :show-close-button="!busy"
    :close-on-escape="!busy"
    @close="cancel"
  >
    <form id="delete-account-form" class="space-y-4" @submit.prevent="confirm">
      <p class="text-sm text-gray-600 dark:text-gray-400">{{ message }}</p>
      <div>
        <label for="account-delete-note" class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.accounts.deleteNote') }}
        </label>
        <textarea
          id="account-delete-note"
          v-model="note"
          rows="3"
          maxlength="1000"
          class="input w-full resize-y"
          :placeholder="t('admin.accounts.deleteNotePlaceholder')"
          :disabled="busy"
        />
      </div>
    </form>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="busy" @click="cancel">{{ t('common.cancel') }}</button>
        <button type="submit" form="delete-account-form" class="btn btn-danger" :disabled="busy || note.length > 1000">
          {{ t(busy ? 'common.processing' : 'common.delete') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'

const props = defineProps<{ show: boolean; message: string; busy: boolean }>()
const emit = defineEmits<{ confirm: [note: string]; cancel: [] }>()
const { t } = useI18n()
const note = ref('')

watch(() => props.show, () => { note.value = '' })

function cancel() {
  if (!props.busy) emit('cancel')
}

function confirm() {
  if (!props.busy && note.value.length <= 1000) emit('confirm', note.value.trim())
}
</script>
