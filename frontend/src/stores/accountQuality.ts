import { defineStore } from 'pinia'
import { ref, watch } from 'vue'
import { useAuthStore } from './auth'
import { listQualityPlans, listQualityOperations, listQualityTemplates, listQualityPresets, type QualityOperation, type QualityOperationFilter, type QualityRuleTemplate, type QualityRulePreset } from '@/api/admin/accountQuality'
import { getAllIncludingInactive } from '@/api/admin/groups'
import type { AdminGroup, ScheduledTestPlan } from '@/types'

// Session-only summaries survive route navigation. Answer bodies stay in the detail drawer.
export const useAccountQualityStore = defineStore('accountQuality', () => {
  const auth = useAuthStore()
  const plans = ref<ScheduledTestPlan[]>([]), groups = ref<AdminGroup[]>([]), templates = ref<QualityRuleTemplate[]>([])
  const presets = ref<QualityRulePreset[]>([]), presetsLoading = ref(false), presetsError = ref('')
  const operations = ref<QualityOperation[]>([]), cursor = ref(0)
  const rulesLoaded = ref(false), operationsLoaded = ref(false)
  const rulesLoading = ref(false), operationsLoading = ref(false), moreLoading = ref(false)
  const rulesError = ref(''), operationsError = ref(''), groupsError = ref('')
  const updatedAt = ref(0)
  const search = ref(''), selectedPlanId = ref<number | null>(null), operationFilter = ref('all')
  const selectedTemplateId = ref<number | null>(null), unassignedOnly = ref(false)
  let changingScope = false
  let ruleVersion = 0, operationVersion = 0, groupVersion = 0, presetVersion = 0
  let rulesFlight: Promise<void> | null = null, groupsFlight: Promise<void> | null = null
  const errorText = (e: unknown) => e instanceof Error ? e.message : 'qualityOps.error'

  function reset() {
    ruleVersion++; operationVersion++; groupVersion++; presetVersion++
    presets.value = []; presetsLoading.value = false; presetsError.value = ''
    plans.value = []; groups.value = []; templates.value = []; operations.value = []; cursor.value = 0
    rulesLoaded.value = operationsLoaded.value = false
    rulesLoading.value = operationsLoading.value = moreLoading.value = false
    rulesError.value = operationsError.value = groupsError.value = ''
    changingScope = true
    try {
      search.value = ''; selectedPlanId.value = null; selectedTemplateId.value = null; unassignedOnly.value = false
      operationFilter.value = 'all'; updatedAt.value = 0
    } finally { changingScope = false }
    rulesFlight = groupsFlight = null
  }
  async function refreshPresets() {
    const version = ++presetVersion
    presetsLoading.value = true; presetsError.value = ''
    try {
      const data = await listQualityPresets()
      if (version === presetVersion) presets.value = data
    } catch (e) { if (version === presetVersion) presetsError.value = errorText(e) }
    finally { if (version === presetVersion) presetsLoading.value = false }
  }
  watch(() => auth.user ? `${auth.user.id}:${auth.user.role}` : '', reset, { flush: 'sync' })
  watch(selectedPlanId, () => {
    if (changingScope) return
    clearOperations()
    void refreshOperations()
  }, { flush: 'sync' })

  function clearOperations() {
    operationVersion++
    operations.value = []; cursor.value = 0; operationsLoaded.value = false
    operationsLoading.value = moreLoading.value = false; operationsError.value = ''
  }
  async function selectProbe(templateId: number | null, unassigned = false) {
    changingScope = true
    try {
      selectedTemplateId.value = templateId; unassignedOnly.value = templateId === null && unassigned
      selectedPlanId.value = null; search.value = ''; operationFilter.value = 'all'
      clearOperations()
    } finally { changingScope = false }
    await refreshOperations()
  }

  function operationScope(): QualityOperationFilter {
    const filter: QualityOperationFilter = {}
    if (selectedTemplateId.value !== null) filter.template_id = selectedTemplateId.value
    else if (unassignedOnly.value) filter.unassigned = true
    if (selectedPlanId.value !== null) filter.plan_id = selectedPlanId.value
    return filter
  }

  function refreshRules(force = false): Promise<void> {
    if (rulesFlight && !force) return rulesFlight
    const version = ++ruleVersion
    rulesLoading.value = true; rulesError.value = ''
    const flight = (async () => {
      try {
        // 分组规则和它建出的账号规则一起刷新，卡片上的覆盖数和规则列表不会对不上。
        const [data, groupRules] = await Promise.all([listQualityPlans(), listQualityTemplates()])
        if (version !== ruleVersion) return
        const firstLoad = !rulesLoaded.value
        plans.value = data; templates.value = groupRules; rulesLoaded.value = true
        updatedAt.value = Date.now()
        if ((firstLoad && selectedTemplateId.value === null && !unassignedOnly.value && groupRules.length > 0) ||
          (selectedTemplateId.value !== null && !groupRules.some(t => t.id === selectedTemplateId.value))) {
          await selectProbe(groupRules[0]?.id ?? null, groupRules.length === 0)
        } else if (selectedPlanId.value !== null) {
          const selected = data.find(p => p.id === selectedPlanId.value)
          const template = groupRules.find(t => t.id === selectedTemplateId.value)
          if (!selected || (selectedTemplateId.value !== null &&
            (!template?.plan_ids.includes(selected.id) || (selected.source_template_id && selected.source_template_id !== selectedTemplateId.value)))) {
            changingScope = true
            try { selectedPlanId.value = null; clearOperations() }
            finally { changingScope = false }
            await refreshOperations()
          }
        }
      } catch (e) { if (version === ruleVersion) rulesError.value = errorText(e) }
      finally { if (version === ruleVersion) { rulesLoading.value = false; rulesFlight = null } }
    })()
    rulesFlight = flight
    return flight
  }
  function refreshGroups(): Promise<void> {
    if (groupsFlight) return groupsFlight
    const version = ++groupVersion
    groupsError.value = ''
    const flight = (async () => {
      try { const data = await getAllIncludingInactive(); if (version === groupVersion) groups.value = data }
      catch (e) { if (version === groupVersion) groupsError.value = errorText(e) }
      finally { if (version === groupVersion) groupsFlight = null }
    })()
    groupsFlight = flight
    return flight
  }
  async function refreshOperations(append = false) {
    if (append && (moreLoading.value || operationsLoading.value || !cursor.value)) return
    const version = ++operationVersion
    if (append) moreLoading.value = true
    else { operationsLoading.value = true; moreLoading.value = false }
    operationsError.value = ''
    try {
      const filter = operationScope(), beforeId = append ? cursor.value : 0
      const page = await (Object.keys(filter).length ? listQualityOperations(beforeId, filter) : listQualityOperations(beforeId))
      if (version !== operationVersion) return
      const unique = new Map<number, QualityOperation>()
      for (const item of [...(append ? operations.value : []), ...page.items]) unique.set(item.id, item)
      operations.value = [...unique.values()]; cursor.value = page.next_cursor
      operationsLoaded.value = true; updatedAt.value = Date.now()
    } catch (e) { if (version === operationVersion) operationsError.value = errorText(e) }
    finally { if (version === operationVersion) operationsLoading.value = moreLoading.value = false }
  }
  async function refresh() {
    const version = operationVersion
    const refreshRecords = async () => {
      await refreshRules()
      // Scope reconciliation already refreshes its records; avoid a second fetch.
      if (version === operationVersion) await refreshOperations()
    }
    await Promise.all([refreshRecords(), refreshGroups(), refreshPresets()])
  }
  return { plans, groups, templates, operations, cursor, rulesLoaded, operationsLoaded, rulesLoading, operationsLoading, moreLoading,
    rulesError, operationsError, groupsError, updatedAt, search, selectedPlanId, selectedTemplateId, unassignedOnly, operationFilter,
    presets, presetsLoading, presetsError, refreshPresets,
    refresh, refreshRules, refreshGroups, refreshOperations, selectProbe, reset }
})
