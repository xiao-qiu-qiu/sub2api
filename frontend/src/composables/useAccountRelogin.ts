import { onUnmounted, reactive } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { getAccountReloginStatus, reloginTokenGuardV2Account, type TokenGuardV2Task } from '@/api/admin/accountTokenGuardV2'
import { extractApiErrorCode } from '@/utils/apiError'
import { taskOperationFailure } from '@/utils/credentialOperationFailure'
import type { Account } from '@/types'

type ReloginAccount = Pick<Account, 'id' | 'name' | 'platform' | 'type' | 'parent_account_id'>
interface ReloginOperation {
  account: Pick<ReloginAccount, 'id' | 'name'>
  taskId: number
  deadline: number
  errors: number
  controller: AbortController
  timer?: ReturnType<typeof setTimeout>
}

const pollInterval = 2_000
const maxWait = 15 * 60_000
const manualErrorKeys: Record<string, string> = {
  TOKEN_GUARD_V2_NOT_FOUND: 'admin.accounts.reloginConfigRequired',
  TOKEN_GUARD_V2_PROBE_BUSY: 'tokenGuardV2.operationFailure.manual.busy',
  OPENAI_REAUTH_TASK_ACTIVE: 'tokenGuardV2.operationFailure.manual.busy',
  OPENAI_REAUTH_TASK_ALREADY_ACTIVE: 'tokenGuardV2.operationFailure.manual.busy',
  OPENAI_REAUTH_WORKER_UNAVAILABLE: 'tokenGuardV2.operationFailure.manual.worker',
  OPENAI_REAUTH_MODE_UNAVAILABLE: 'tokenGuardV2.operationFailure.manual.worker',
  OPENAI_REAUTH_UNAVAILABLE: 'tokenGuardV2.operationFailure.manual.worker',
  OPENAI_REAUTH_CONFIG_REQUIRED: 'admin.accounts.reloginConfigRequired',
  OPENAI_REAUTH_ENCRYPTION_KEY_REQUIRED: 'tokenGuardV2.operationFailure.manual.encryption',
  OPENAI_REAUTH_ACCOUNT_INVALID: 'admin.accounts.reloginUnsupported',
  OPENAI_REAUTH_ACCOUNT_CHANGED: 'tokenGuardV2.operationFailure.manual.changed',
  OPENAI_REAUTH_IDENTITY_MISMATCH: 'tokenGuardV2.operationFailure.manual.changed',
  OPENAI_REAUTH_TASK_NOT_ACTIVE: 'tokenGuardV2.operationFailure.manual.changed'
}

export function useAccountRelogin(onCompleted?: () => void) {
  const { t } = useI18n()
  const appStore = useAppStore()
  const pendingIds = reactive(new Set<number>())
  const operations = new Map<number, ReloginOperation>()
  let disposed = false

  const isCurrent = (operation: ReloginOperation) => !disposed && operations.get(operation.account.id) === operation
  const errorMessage = (error: unknown) => t(manualErrorKeys[extractApiErrorCode(error) ?? ''] ?? 'tokenGuardV2.error')
  const finish = (operation: ReloginOperation) => {
    clearTimeout(operation.timer)
    operation.controller.abort()
    operations.delete(operation.account.id)
    pendingIds.delete(operation.account.id)
  }
  const unconfirmed = (operation: ReloginOperation, reason: string) => {
    finish(operation)
    appStore.showError(t('admin.accounts.reloginResultUnavailable', { name: operation.account.name, reason }))
  }
  const applyTask = (operation: ReloginOperation, task: TokenGuardV2Task): boolean => {
    if (task.id !== operation.taskId || task.account_id !== operation.account.id) {
      unconfirmed(operation, t('admin.accounts.reloginTaskChanged'))
      return true
    }
    if (task.status === 'succeeded') {
      finish(operation)
      appStore.showSuccess(t('admin.accounts.reloginSuccess', { name: operation.account.name }))
      onCompleted?.()
      return true
    }
    if (task.status === 'failed') {
      const failure = taskOperationFailure(task)!
      const reason = `${t(`tokenGuardV2.operationFailure.reasons.${failure.category}`)} ${t(`tokenGuardV2.operationFailure.actions.${failure.action}`)}`
      finish(operation)
      appStore.showError(t('admin.accounts.reloginFailed', { name: operation.account.name, reason }))
      onCompleted?.()
      return true
    }
    return false
  }
  const schedule = (operation: ReloginOperation) => {
    operation.timer = setTimeout(() => { void poll(operation) }, pollInterval)
  }
  const poll = async (operation: ReloginOperation) => {
    if (!isCurrent(operation)) return
    if (Date.now() >= operation.deadline) {
      unconfirmed(operation, t('admin.accounts.reloginResultTimeout'))
      return
    }
    try {
      const { task } = await getAccountReloginStatus(operation.account.id, operation.controller.signal)
      if (!isCurrent(operation)) return
      if (!task) {
        unconfirmed(operation, t('admin.accounts.reloginTaskChanged'))
        return
      }
      operation.errors = 0
      if (applyTask(operation, task)) return
    } catch (error) {
      if (!isCurrent(operation)) return
      if (++operation.errors >= 3) {
        unconfirmed(operation, errorMessage(error))
        return
      }
    }
    schedule(operation)
  }
  const relogin = async (account: ReloginAccount) => {
    if (disposed || pendingIds.has(account.id)) return
    if (account.platform !== 'openai' || account.type !== 'oauth' || account.parent_account_id != null) {
      appStore.showError(t('admin.accounts.reloginFailed', { name: account.name, reason: t('admin.accounts.reloginUnsupported') }))
      return
    }
    const operation: ReloginOperation = {
      account: { id: account.id, name: account.name }, taskId: 0, deadline: Date.now() + maxWait, errors: 0, controller: new AbortController()
    }
    operations.set(account.id, operation)
    pendingIds.add(account.id)
    try {
      const task = await reloginTokenGuardV2Account(account.id, operation.controller.signal)
      if (!isCurrent(operation)) return
      operation.taskId = task.id
      if (applyTask(operation, task)) return
      appStore.showInfo(t('admin.accounts.reloginStarted', { name: account.name }))
      schedule(operation)
    } catch (error) {
      if (!isCurrent(operation)) return
      finish(operation)
      appStore.showError(t('admin.accounts.reloginFailed', { name: account.name, reason: errorMessage(error) }))
    }
  }

  onUnmounted(() => {
    disposed = true
    for (const operation of operations.values()) {
      clearTimeout(operation.timer)
      operation.controller.abort()
    }
    operations.clear()
    pendingIds.clear()
  })

  return { relogin, isRelogging: (accountId: number) => pendingIds.has(accountId) }
}
