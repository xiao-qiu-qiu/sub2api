<template>
  <BaseDialog
    :show="true"
    :title="t(ending ? 'admin.settings.accountRotation.endManual' : 'admin.settings.accountRotation.manualTitle')"
    width="narrow"
    :show-close-button="!saving"
    :close-on-escape="!saving"
    @close="close"
  >
    <form id="manual-account-rest-form" @submit.prevent="confirm">
      <p class="text-sm leading-relaxed text-gray-600 dark:text-gray-300">
        {{ t(ending ? 'admin.settings.accountRotation.endManualConfirm' : 'admin.settings.accountRotation.manualConfirm', { name: account.name }) }}
      </p>
      <label v-if="!ending" for="manual-rest-seconds" class="mt-5 block text-sm font-medium text-gray-700 dark:text-gray-300">
        {{ t('admin.settings.accountRotation.manualSeconds') }}
        <input id="manual-rest-seconds" v-model.number="seconds" type="number" min="1" max="7200" step="1" required :disabled="saving" class="input mt-2 min-h-11 w-full" />
      </label>
      <p v-if="error" role="alert" class="mt-3 text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    </form>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="close">{{ t('common.cancel') }}</button>
        <button type="submit" form="manual-account-rest-form" class="btn btn-primary" :disabled="saving || !valid">
          {{ saving ? t('common.processing') : t(ending ? 'admin.settings.accountRotation.endManual' : 'admin.settings.accountRotation.confirmManual') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { extractApiErrorMessage } from '@/utils/apiError'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { Account } from '@/types'

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{
  close: []
  updated: [state: NonNullable<Account['account_rotation']>, ended: boolean]
}>()
const { t } = useI18n()
// Capture the selected action. A background list refresh must not change what
// the confirmation button will do while the dialog is open.
const ending = props.account.account_rotation?.hold === true && props.account.account_rotation.reason === 'manual'
const seconds = ref<number>(60)
const saving = ref(false)
const error = ref('')
const valid = computed(() => ending || (Number.isInteger(seconds.value) && seconds.value >= 1 && seconds.value <= 7200))
const close = () => { if (!saving.value) emit('close') }
const confirm = async () => {
  if (saving.value || !valid.value) return
  saving.value = true
  error.value = ''
  try {
    const result = ending
      ? await adminAPI.accounts.cancelManualRest(props.account.id)
      : await adminAPI.accounts.startManualRest(props.account.id, seconds.value)
    emit('updated', result.account_rotation, ending)
    emit('close')
  } catch (cause) {
    error.value = extractApiErrorMessage(cause, t('admin.settings.accountRotation.manualFailed'))
  } finally {
    saving.value = false
  }
}
</script>
