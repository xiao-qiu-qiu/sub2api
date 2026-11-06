<template>
  <nav class="quality-tabs" :aria-label="t('qualityOps.accountQuality')">
    <RouterLink to="/admin/account-quality/status" :class="{ active: status }" :aria-current="status ? 'page' : undefined"><Icon name="chartBar" size="sm" />{{ t('qualityOps.statusTitle') }}</RouterLink>
    <RouterLink to="/admin/account-quality" :class="{ active: !status }" :aria-current="!status ? 'page' : undefined"><Icon name="cog" size="sm" />{{ t('qualityOps.title') }}</RouterLink>
  </nav>
</template>

<script setup lang="ts">
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
defineProps<{ status?: boolean }>()
const { t } = useI18n()
</script>

<style scoped>
.quality-tabs { @apply flex gap-6 border-b border-gray-200 dark:border-dark-700; }
.quality-tabs a { @apply -mb-px inline-flex min-h-11 items-center gap-2 border-b-2 border-transparent px-1 py-3 text-sm font-medium text-gray-500 transition-colors hover:text-gray-900 dark:text-gray-400 dark:hover:text-white; }
.quality-tabs a.active { @apply border-primary-500 text-primary-600 dark:text-primary-400; }
</style>
