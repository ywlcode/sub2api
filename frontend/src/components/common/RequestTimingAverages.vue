<template>
  <dl class="min-w-0 space-y-2" :title="t('usage.requestTimingAveragesHint', { count: stats?.timing_sample_count ?? 0 })">
    <div data-testid="average-user-upload">
      <dt class="text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('usage.avgUserUpload') }}</dt>
      <dd class="text-xl font-bold tabular-nums text-gray-900 dark:text-white">{{ formatRequestPhaseDuration(stats?.average_user_upload_ms) }}</dd>
    </div>
    <div data-testid="average-processing">
      <dt class="text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('usage.avgProcessing') }}</dt>
      <dd class="text-xl font-bold tabular-nums text-gray-900 dark:text-white">{{ formatRequestPhaseDuration(stats?.average_processing_ms) }}</dd>
    </div>
  </dl>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { RequestPhaseAverages } from '@/types/usageTiming'
import { formatRequestPhaseDuration } from '@/utils/usageRequestTiming'

defineProps<{ stats: RequestPhaseAverages | null | undefined }>()
const { t } = useI18n()
</script>
