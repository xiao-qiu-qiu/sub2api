<template>
  <AppLayout>
    <div class="quality-status-page">
      <QualityTabs status />
      <header class="status-heading">
        <div><h1>{{ t('qualityOps.statusTitle') }}<span class="live-mark" aria-hidden="true" /></h1></div>
        <div class="heading-actions"><span v-if="updatedAt" class="updated-at">{{ t('qualityOps.statusUpdated', { time: time(updatedAt) }) }}</span><button class="btn btn-secondary" :disabled="loading" @click="load"><Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />{{ t('qualityOps.statusRefresh') }}</button></div>
      </header>

      <section class="status-summary" :aria-label="t('qualityOps.statusPageScope')">
        <div v-for="status in statuses" :key="status" class="summary-stat" :class="`stat-${status}`"><span class="stat-dot" /><div><span>{{ label(status) }}</span><strong>{{ loaded ? accounts.filter(account => account.status === status).length : '—' }}</strong></div></div>
        <span class="summary-scope">{{ t('qualityOps.statusPageScope') }}</span>
      </section>

      <section class="status-panel filter-panel">
        <div class="status-toolbar">
          <form class="search-field" role="search" @submit.prevent="applyFilters"><Icon name="search" size="sm" /><input v-model="search" :placeholder="t('qualityOps.statusSearch')" :aria-label="t('qualityOps.statusSearch')" /><button v-if="search" type="button" :aria-label="t('qualityOps.clearSearch')" @click="search = ''; applyFilters()"><Icon name="x" size="sm" /></button></form>
          <select v-model="group" class="input group-filter" :aria-label="t('qualityOps.statusAllGroups')" @change="applyFilters"><option value="">{{ t('qualityOps.statusAllGroups') }}</option><option v-for="item in groups" :key="item.id" :value="String(item.id)">{{ item.name }}</option></select>
          <div class="channel-switch" :aria-label="t('qualityOps.statusChannel')"><button v-for="value in (['account', 'bps'] as const)" :key="value" :class="{ selected: channel === value }" :aria-pressed="channel === value" @click="channel = value; applyFilters()">{{ t(value === 'account' ? 'qualityOps.statusAccountChannel' : 'qualityOps.statusBpsChannel') }}</button></div>
        </div>
      </section>
      <div v-if="notice" class="status-message notice" role="status">{{ notice }}<button :aria-label="t('qualityOps.dismiss')" @click="notice = ''"><Icon name="x" size="sm" /></button></div>
      <section v-for="section in sections" :key="section.scope" class="status-panel" :data-probe-scope="section.scope" :aria-label="scopeTitle(section.scope)">
        <header class="scope-heading"><div><span class="scope-icon" :class="section.scope"><Icon :name="section.scope === 'main' ? 'shield' : 'sync'" size="sm" /></span><h2>{{ scopeTitle(section.scope) }}</h2><span class="scope-count">{{ section.loaded ? section.total : '—' }}</span></div><RouterLink class="scope-configure" :to="configureRoute(section.scope)"><Icon name="plus" size="xs" />{{ t(section.scope === 'recovery' ? 'qualityOps.recoveryQuickSetup' : 'qualityOps.create') }}</RouterLink></header>
        <div v-if="section.error" class="status-message error" role="alert">{{ section.error }}<button @click="load(section.scope)">{{ t('qualityOps.retry') }}</button></div>
        <div class="list-caption"><span>{{ t('qualityOps.statusHistory') }}</span><div class="legend"><span v-for="status in statuses.slice(0, 3)" :key="status"><i :class="`bar-${status}`" />{{ label(status) }}</span></div></div>
        <div :aria-busy="section.loading" class="account-list">
          <div v-if="section.loading && !section.loaded" class="loading-rows" role="status" :aria-label="t('qualityOps.loading')"><div v-for="i in 5" :key="i" class="skeleton-row"><span /><span /></div></div>
          <article v-for="account in section.accounts" :key="account.account_id" class="account-row" :data-account-id="account.account_id">
            <div class="account-identity"><div class="avatar" :class="`avatar-${account.status}`">{{ account.account_name.slice(0, 1).toUpperCase() || '#' }}</div><div class="identity-copy"><button class="account-name" :title="account.account_name" @click="openRules(account)">{{ account.account_name || `#${account.account_id}` }}</button><div class="account-meta"><span>#{{ account.account_id }}</span><span>{{ account.platform }}</span><span class="state-label" :class="`text-${account.status}`">{{ label(account.status) }}</span></div><p class="account-groups" :title="account.groups.map(item => item.name).join(' / ')">{{ account.groups.map(item => item.name).join(' / ') || t('qualityOps.ungroupedAccounts') }}</p></div></div>
            <div class="history-cell">
              <div v-if="account.history.length" class="history-bars" :aria-label="t('qualityOps.statusOpenHistory', { account: account.account_name })">
                <span v-for="n in Math.max(0, 60 - account.history.length)" :key="`empty-${n}`" class="history-bar bar-empty" aria-hidden="true" />
                <button v-for="point in account.history" :key="point.id" class="history-bar" :class="`bar-${point.status}`" :title="pointTitle(point)" :aria-label="pointTitle(point)" @click="selected = { account, point }" />
              </div>
              <div v-else class="empty-history"><span v-for="n in 60" :key="n" class="history-bar bar-empty" /><span class="empty-history-label">{{ t('qualityOps.statusNoHistory') }}</span></div>
              <div class="history-meta"><span>{{ account.history.length ? date(account.history[0].started_at) : '—' }}</span><span v-if="recoveryPlan(account)" class="recovery-label" :title="t('qualityOps.statusRecoveryFlow', { interval: recoveryPlan(account)?.recovery_probe_interval_minutes || 60, confirmation: recoveryPlan(account)?.recovery_confirmation_minutes || 5 })"><Icon name="sync" size="xs" />{{ t('qualityOps.statusRecovery') }} · {{ recoveryPlan(account)?.recovery_probe_interval_minutes || 60 }} / {{ recoveryPlan(account)?.recovery_confirmation_minutes || 5 }} {{ t('qualityOps.minutesUnit') }}</span><span>{{ account.history.length ? date(account.history[account.history.length - 1].started_at) : '—' }}</span></div>
            </div>
            <div class="rate-cell" :title="t('qualityOps.statusRateHelp')"><strong :class="{ 'rate-healthy': account.healthy_rate === 100 }">{{ account.healthy_rate === null ? '—' : `${Math.round(account.healthy_rate)}%` }}</strong><span>{{ t('qualityOps.statusHealthyRate') }}</span></div>
            <div class="row-actions"><button class="probe-button" :disabled="!runnablePlan(account) || !!pending[runnablePlan(account)?.id || 0]" :title="account.plans.length ? undefined : t('qualityOps.statusNoRule')" @click="probe(account)"><Icon name="play" size="xs" />{{ pending[runnablePlan(account)?.id || 0] ? t('qualityOps.statusQueued') : t('qualityOps.statusProbe') }}</button><button class="rule-button" :aria-label="`${t('qualityOps.statusConfigure')} · ${account.account_name}`" @click="openRules(account, true)"><Icon name="cog" size="sm" /></button></div>
          </article>
          <div v-if="section.loaded && !section.accounts.length" class="empty-state"><Icon name="search" size="lg" /><p>{{ emptyScopeMessage(section.scope) }}</p></div>
        </div>
        <footer class="list-footer"><span>{{ t('qualityOps.statusTotal', { count: section.total }) }}</span><div><button :disabled="section.page <= 1 || section.loading" :aria-label="t('qualityOps.statusPrev')" @click="section.page--; load(section.scope)"><Icon name="chevronLeft" size="sm" /></button><span>{{ t('qualityOps.statusPage', { page: section.page, pages: pages(section) }) }}</span><button :disabled="section.page >= pages(section) || section.loading" :aria-label="t('qualityOps.statusNextPage')" @click="section.page++; load(section.scope)"><Icon name="chevronRight" size="sm" /></button></div></footer>
      </section>
    </div>

    <BaseDialog :show="!!selected" :title="t('qualityOps.statusEvidence')" @close="selected = null">
      <p v-if="selected?.point.route_evidence_missing" class="mb-4 text-sm text-gray-500" data-testid="quality-missing-route">{{ t('qualityOps.statusMissingRouteEvidence') }}</p>
      <template v-if="selected"><div class="evidence-account">{{ selected.account.account_name }}<span class="state-label" :class="`text-${selected.point.status}`">{{ label(selected.point.status) }}</span></div><dl class="evidence-details"><div><dt>{{ t('qualityOps.statusSampleAt') }}</dt><dd>{{ new Date(selected.point.started_at).toLocaleString() }}</dd></div><div><dt>{{ t('qualityOps.statusSampleModel') }}</dt><dd>{{ selected.point.model_ids?.join(' / ') || selected.point.model_id }}</dd></div><div><dt>{{ t('qualityOps.statusSampleMethod') }}</dt><dd>{{ t(selected.point.question_kind === 'state_probe' ? 'qualityOps.questionStateProbe' : selected.point.question_kind === 'random_reasoning' ? 'qualityOps.questionRandom' : 'qualityOps.questionCandy') }}</dd></div><div><dt>{{ t('qualityOps.statusSampleAction') }}</dt><dd>{{ actionLabel(selected.point.quality_action) }}</dd></div></dl><RouterLink class="btn btn-primary inline-flex items-center gap-2" :to="{ path: '/admin/account-quality', query: { plan_id: selected.point.plan_id, history: '1' } }">{{ t('qualityOps.statusViewHistory') }}<Icon name="arrowRight" size="sm" /></RouterLink></template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import QualityTabs from '@/components/admin/operations/QualityTabs.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { getQualityOverview, type QualityOverviewAccount, type QualityHistoryPoint, type QualityStatus, type QualityProbeScope } from '@/api/admin/qualityOverview'
import { runQualityPlan } from '@/api/admin/accountQuality'
import { getAllIncludingInactive } from '@/api/admin/groups'
import { useAuthStore } from '@/stores/auth'
import type { AdminGroup } from '@/types'

const { t, te } = useI18n(), router = useRouter(), auth = useAuthStore()
interface ScopeSection { scope: QualityProbeScope; accounts: QualityOverviewAccount[]; page: number; total: number; loading: boolean; loaded: boolean; error: string }
const sections = ref<ScopeSection[]>(['main', 'recovery'].map(scope => ({ scope: scope as QualityProbeScope, accounts: [], page: 1, total: 0, loading: false, loaded: false, error: '' })))
const accounts = computed(() => [...new Map(sections.value.flatMap(section => section.accounts).map(account => [account.account_id, account])).values()])
const groups = ref<AdminGroup[]>([])
const search = ref(''), group = ref(''), channel = ref<'account' | 'bps'>('account')
const loading = computed(() => sections.value.some(section => section.loading)), loaded = computed(() => sections.value.every(section => section.loaded))
const notice = ref(''), updatedAt = ref('')
const pending = ref<Record<number, boolean>>({}), selected = ref<{ account: QualityOverviewAccount; point: QualityHistoryPoint } | null>(null)
const statuses: QualityStatus[] = ['healthy', 'degraded', 'unknown', 'untested']
const pages = (section: ScopeSection) => Math.max(1, Math.ceil(section.total / 20))
let alive = true
const requests = { main: 0, recovery: 0 }, controllers: Partial<Record<QualityProbeScope, AbortController>> = {}
let poll: ReturnType<typeof setInterval> | undefined, debounce: ReturnType<typeof setTimeout> | undefined
const statusKeys = { healthy: 'statusHealthy', degraded: 'statusDegraded', unknown: 'statusUnknown', untested: 'statusUntested' }
const scopeTitle = (scope: QualityProbeScope) => t(channel.value === 'bps' ? (scope === 'main' ? 'qualityOps.bpsProbeScope' : 'qualityOps.bpsRecoveryScope') : (scope === 'main' ? 'qualityOps.mainProbeScope' : 'qualityOps.recoveryProbeScope'))
const emptyScopeMessage = (scope: QualityProbeScope) => t(channel.value === 'bps' ? (scope === 'main' ? 'qualityOps.noBpsProbeScope' : 'qualityOps.noBpsRecoveryScope') : (scope === 'main' ? 'qualityOps.noMainScope' : 'qualityOps.noRecoveryScope'))
function configureRoute(scope: QualityProbeScope) {
  if (channel.value === 'bps') return { path: '/admin/account-quality', query: scope === 'main' ? { create: '1', channel: 'bps' } : { recovery: '1', channel: 'bps' } }
  return { path: '/admin/account-quality', query: scope === 'recovery' ? { recovery: '1' } : { create: '1' } }
}
const label = (status: QualityStatus) => t(`qualityOps.${statusKeys[status]}`)
const date = (value: string) => new Date(value).toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false })
const time = (value: string) => new Date(value).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
const message = (cause: unknown) => { const err = cause as { response?: { data?: { message?: string } }; message?: string }; return err.response?.data?.message || err.message || t('qualityOps.error') }
const pointTitle = (point: QualityHistoryPoint) => `${date(point.started_at)} · ${label(point.status)} · ${point.model_id}`
const recoveryPlan = (account: QualityOverviewAccount) => account.plans.find(plan => (plan.test_channel || 'account') === channel.value && plan.enabled && (plan.action_on === 'passed' || (plan.auto_restore && plan.recovery_probe_enabled)))
function runnablePlan(account: QualityOverviewAccount) {
  const candidates = account.plans.filter(plan => (plan.test_channel || 'account') === channel.value && plan.enabled && (!plan.running_until || Date.parse(plan.running_until) <= Date.now()))
  return candidates.find(plan => ['move_groups', 'remove_groups', 'disable_scheduling', 'isolate_models', 'remove_models'].includes(plan.action)) || candidates[0]
}
function actionLabel(action: string) { return action === 'recovery_confirmation_pending' ? t('qualityOps.recoveryAwaitingConfirmation') : te(`qualityOps.outcomes.${action}`) ? t(`qualityOps.outcomes.${action}`) : '—' }
async function load(scope?: QualityProbeScope | Event) {
  await Promise.all(sections.value.filter(section => typeof scope !== 'string' || section.scope === scope).map(async section => {
    const key = section.scope, version = ++requests[key]
    controllers[key]?.abort(); controllers[key] = new AbortController()
    section.loading = true; section.error = ''
    try {
      const data = await getQualityOverview({ page: section.page, page_size: 20, search: search.value.trim() || undefined, group_id: group.value ? Number(group.value) : undefined, test_channel: channel.value, probe_scope: key }, controllers[key]?.signal)
      if (!alive || version !== requests[key]) return
      section.accounts = data.items; section.total = data.total; section.loaded = true; updatedAt.value = new Date().toISOString()
      if (section.page > pages(section)) { section.page = pages(section); void load(key) }
    } catch (cause) { if (alive && version === requests[key]) section.error = message(cause) }
    finally { if (version === requests[key]) section.loading = false }
  }))
}
function clearResults() {
  for (const section of sections.value) { section.page = 1; section.accounts = []; section.total = 0; section.loaded = false; section.error = '' }
  selected.value = null
}
function applyFilters() { if (debounce) clearTimeout(debounce); debounce = undefined; clearResults(); void load() }
watch(search, () => { if (debounce) clearTimeout(debounce); debounce = setTimeout(applyFilters, 300) })
function openRules(account: QualityOverviewAccount, edit = false) {
  const candidates = account.plans.filter(plan => (plan.test_channel || 'account') === channel.value)
  const plan = candidates.find(item => !['enable_bps', 'observe_only', 'openai_two_fa_admission'].includes(item.action)) || candidates[0]
  void router.push({ path: '/admin/account-quality', query: plan ? { plan_id: plan.id, ...(edit ? { edit: '1' } : {}) } : { create_account: account.account_id } })
}
async function probe(account: QualityOverviewAccount) {
  const plan = runnablePlan(account)
  if (!plan || pending.value[runnablePlan(account)?.id || 0]) return
  pending.value[plan.id] = true; notice.value = ''
  try { await runQualityPlan(plan.id); if (alive) { notice.value = t('qualityOps.statusProbeQueued'); await load() } }
  catch (cause) { if (alive) { const section = sections.value.find(item => item.accounts.includes(account)); if (section) section.error = message(cause) } }
  finally { if (alive) delete pending.value[plan.id] }
}
watch(() => auth.user?.id, () => { for (const section of sections.value) { ++requests[section.scope]; controllers[section.scope]?.abort(); section.loading = false } clearResults(); groups.value = []; notice.value = '' })
onMounted(() => {
  void load()
  void getAllIncludingInactive().then(data => { if (alive) groups.value = data }).catch(() => {})
  poll = setInterval(() => { if (document.visibilityState === 'visible' && !loading.value && !debounce) void load() }, 30_000)
})
onBeforeUnmount(() => { alive = false; for (const section of sections.value) { ++requests[section.scope]; controllers[section.scope]?.abort() } if (poll) clearInterval(poll); if (debounce) clearTimeout(debounce) })
</script>

<style scoped>
.quality-status-page { @apply flex min-w-0 flex-col gap-5 text-gray-900 dark:text-gray-100; }
.quality-status-page > nav { margin-bottom: 0; max-width: 100%; }
.status-heading { @apply flex flex-wrap items-center justify-between gap-4 pt-1; }
.status-heading h1 { @apply flex items-center gap-3 text-2xl font-semibold tracking-tight; }
.status-heading p { @apply mt-2 text-sm text-gray-500 dark:text-gray-400; }
.live-mark { @apply h-2 w-2 rounded-full bg-emerald-500 ring-4 ring-emerald-500/10; }
.heading-actions { @apply flex items-center gap-3; }
.heading-actions .btn { @apply inline-flex items-center gap-2; }
.updated-at { @apply hidden text-xs tabular-nums text-gray-500 xl:block; }
.status-summary { @apply relative grid grid-cols-2 gap-y-5 rounded-2xl border border-gray-200/80 bg-white px-6 py-5 shadow-sm dark:border-dark-700 dark:bg-dark-900 sm:grid-cols-4; }
.summary-stat { @apply flex items-start gap-3; }
.stat-dot { @apply mt-1.5 h-2.5 w-2.5 rounded-full bg-gray-300 dark:bg-dark-500; }
.stat-healthy .stat-dot { @apply bg-emerald-500; }
.stat-degraded .stat-dot { @apply bg-rose-500; }
.stat-unknown .stat-dot { @apply bg-slate-400; }
.summary-stat div > span { @apply text-xs text-gray-500 dark:text-gray-400; }
.summary-stat strong { @apply mt-1 block text-3xl font-semibold tabular-nums tracking-tight; }
.summary-scope { @apply absolute right-4 top-3 text-[10px] text-gray-400; }
.scope-heading { @apply flex flex-wrap items-center justify-between gap-3 px-5 py-4; }
.scope-heading > div { @apply flex items-center gap-2.5; }
.scope-heading h2 { @apply text-sm font-semibold; }
.scope-icon { @apply flex h-8 w-8 items-center justify-center rounded-lg bg-primary-50 text-primary-600 dark:bg-primary-950/40 dark:text-primary-400; }
.scope-icon.recovery { @apply bg-amber-50 text-amber-700 dark:bg-amber-950/30 dark:text-amber-400; }
.scope-count { @apply rounded-md bg-gray-100 px-2 py-0.5 text-xs tabular-nums text-gray-500 dark:bg-dark-700 dark:text-gray-400; }
.scope-configure { @apply inline-flex min-h-9 items-center gap-1.5 text-xs font-medium text-primary-600 dark:text-primary-400; }
.status-panel { @apply overflow-hidden rounded-2xl border border-gray-200/80 bg-white shadow-sm dark:border-dark-700 dark:bg-dark-900; }
.status-toolbar { @apply flex flex-wrap items-center gap-3 px-5 py-4; }
.search-field { @apply flex h-11 min-w-0 flex-1 items-center gap-2 rounded-lg border border-gray-200 px-3 text-gray-400 dark:border-dark-600; min-width: 190px; }
.search-field input { @apply w-full min-w-0 bg-transparent text-sm text-gray-900 outline-none dark:text-gray-100; }
.search-field button { @apply flex h-8 w-8 shrink-0 items-center justify-center; }
.group-filter { @apply h-11 w-auto; max-width: 220px; }
.channel-switch { @apply flex shrink-0 rounded-lg bg-gray-100 p-1 dark:bg-dark-800; }
.channel-switch button { @apply min-h-9 rounded-md px-3 text-xs font-medium text-gray-500 transition-colors dark:text-gray-400; }
.channel-switch button.selected { @apply bg-white text-gray-900 shadow-sm dark:bg-dark-600 dark:text-white; }
.list-caption { @apply flex flex-wrap items-center justify-between gap-2 border-y border-gray-100 bg-gray-50/70 px-5 py-3 text-xs text-gray-500 dark:border-dark-700 dark:bg-dark-800/40 dark:text-gray-400; }
.legend { @apply flex gap-4; }
.legend > span { @apply inline-flex items-center gap-1.5; }
.legend i { @apply h-2 w-2 rounded-sm; }
.account-row { display: grid; grid-template-columns: minmax(170px, 220px) minmax(160px, 1fr) 66px 116px; @apply items-center gap-5 border-b border-gray-100 px-5 py-3 transition-colors last:border-b-0 hover:bg-gray-50/70 dark:border-dark-700/70 dark:hover:bg-dark-800/35; }
.account-identity { @apply flex min-w-0 items-center gap-3; }
.avatar { @apply flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-gray-100 text-sm font-semibold text-gray-500 dark:bg-dark-800 dark:text-gray-400; }
.avatar-healthy { @apply bg-emerald-50 text-emerald-700 dark:bg-emerald-950/50 dark:text-emerald-400; }
.avatar-degraded { @apply bg-rose-50 text-rose-700 dark:bg-rose-950/40 dark:text-rose-400; }
.identity-copy { @apply min-w-0; }
.account-name { @apply block max-w-full truncate text-left text-sm font-semibold hover:text-primary-600; }
.account-meta { @apply mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-gray-500 dark:text-gray-400; }
.state-label { @apply font-medium; }
.text-healthy { @apply text-emerald-700 dark:text-emerald-400; }
.text-degraded { @apply text-rose-600 dark:text-rose-400; }
.text-unknown, .text-untested { @apply text-gray-500 dark:text-gray-400; }
.account-groups { @apply mt-1 truncate text-[11px] text-gray-400; }
.history-cell { @apply min-w-0; }
.history-bars, .empty-history { display: grid; grid-template-columns: repeat(60, minmax(0, 1fr)); gap: 3px; @apply relative h-8 items-center; }
.history-bar { @apply block h-6 min-w-0 rounded-[3px]; }
button.history-bar { @apply transition-transform hover:scale-y-125 focus-visible:scale-y-125 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-500; }
.bar-healthy { @apply bg-emerald-500 dark:bg-emerald-400; }
.bar-degraded { @apply bg-rose-500 dark:bg-rose-400; }
.bar-unknown { @apply bg-slate-400 dark:bg-slate-500; }
.bar-empty { @apply bg-gray-100 dark:bg-dark-800; }
.empty-history-label { @apply absolute inset-0 flex items-center justify-center text-xs text-gray-500 dark:text-gray-400; }
.history-meta { @apply mt-2 flex items-center justify-between gap-2 text-[10px] tabular-nums text-gray-400; }
.recovery-label { @apply inline-flex items-center gap-1 text-primary-600 dark:text-primary-400; }
.rate-cell { @apply flex flex-col items-end gap-1; }
.rate-cell strong { @apply text-base font-semibold tabular-nums; }
.rate-cell > span { @apply text-[10px] text-gray-400; }
.rate-healthy { @apply text-emerald-700 dark:text-emerald-400; }
.row-actions { @apply flex items-center gap-2; }
.probe-button { @apply inline-flex min-h-11 items-center gap-1.5 rounded-lg bg-gray-100 px-3 text-xs font-medium text-gray-700 transition-colors hover:bg-gray-200 disabled:cursor-not-allowed disabled:opacity-40 dark:bg-dark-700 dark:text-gray-200 dark:hover:bg-dark-600; }
.rule-button { @apply flex h-11 w-9 shrink-0 items-center justify-center rounded-lg text-gray-400 hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-700 dark:hover:text-white; }
.list-footer { @apply flex flex-wrap items-center justify-between gap-3 border-t border-gray-100 px-5 py-3 text-xs text-gray-500 dark:border-dark-700 dark:text-gray-400; }
.list-footer > div { @apply flex items-center gap-3; }
.list-footer button { @apply flex h-10 w-10 items-center justify-center rounded-lg border border-gray-200 hover:bg-gray-100 disabled:opacity-30 dark:border-dark-700 dark:hover:bg-dark-800; }
.status-message { @apply mx-5 mb-3 flex items-center justify-between gap-3 rounded-lg px-4 py-3 text-sm; }
.status-message.error { @apply bg-rose-50 text-rose-700 dark:bg-rose-950/30 dark:text-rose-300; }
.status-message.notice { @apply bg-emerald-50 text-emerald-700 dark:bg-emerald-950/30 dark:text-emerald-300; }
.status-message button { @apply shrink-0 underline; }
.empty-state { @apply flex flex-col items-center gap-3 py-16 text-sm text-gray-400; }
.skeleton-row { @apply flex animate-pulse gap-8 border-b border-gray-100 p-6 dark:border-dark-700; }
.skeleton-row span { @apply h-10 w-40 rounded-lg bg-gray-100 dark:bg-dark-800; }
.skeleton-row span + span { @apply flex-1; }
.recovery-link { @apply flex flex-wrap items-center gap-3 rounded-xl border border-gray-200/70 bg-gray-50/80 px-5 py-4 transition-colors hover:border-primary-300 dark:border-dark-700 dark:bg-dark-900/60 dark:hover:border-primary-700; }
.recovery-link-icon { @apply flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-white text-primary-600 shadow-sm dark:bg-dark-800 dark:text-primary-400; }
.recovery-link > div { @apply flex flex-1 flex-col gap-1; min-width: 180px; }
.recovery-link strong { @apply text-sm font-medium; }
.recovery-link div > span { @apply text-xs text-gray-500 dark:text-gray-400; }
.recovery-cta { @apply ml-auto flex items-center gap-2 text-xs font-medium text-primary-600 dark:text-primary-400; }
.evidence-account { @apply flex flex-wrap items-center justify-between gap-3 break-all text-base font-semibold; }
.evidence-details { @apply my-6 space-y-4 text-sm; }
.evidence-details > div { @apply flex justify-between gap-6; }
.evidence-details dt { @apply shrink-0 text-gray-500 dark:text-gray-400; }
.evidence-details dd { @apply break-all text-right; }
@media (max-width: 1100px) { .account-row { grid-template-columns: minmax(150px, 180px) minmax(120px, 1fr) 55px; gap: 16px; } .row-actions { grid-column: 2 / 4; justify-content: flex-end; margin-top: -8px; } .history-bars, .empty-history { gap: 2px; } }
@media (max-width: 640px) { .quality-status-page > .status-ops-nav { flex-wrap: nowrap; overflow-x: auto; width: 100%; } .status-ops-nav :deep(a) { flex-shrink: 0; } }
@media (max-width: 640px) { .quality-status-page { gap: 16px; } .status-heading p { max-width: 250px; } .status-toolbar { gap: 8px; padding: 16px; } .search-field { flex-basis: 100%; } .group-filter { flex: 1; min-width: 0; width: 0; } .channel-switch button { padding: 0 8px; } .account-row { grid-template-columns: minmax(0, 1fr) auto; gap: 12px; padding: 18px 16px; } .account-identity { grid-column: 1; grid-row: 1; } .rate-cell { grid-column: 2; grid-row: 1; } .history-cell { grid-column: 1 / 3; grid-row: 2; } .row-actions { grid-column: 1 / 3; grid-row: 3; margin-top: 0; } .legend { gap: 10px; } .list-caption { padding: 12px 16px; } .recovery-label { font-size: 9px; } .summary-scope { top: 8px; right: 12px; } }
</style>
