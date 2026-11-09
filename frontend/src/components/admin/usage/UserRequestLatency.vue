<template>
  <component :is="clickable ? 'button' : 'div'" :type="clickable ? 'button' : undefined"
    class="grid grid-cols-[max-content_max-content] items-baseline gap-x-3 gap-y-0.5 text-left text-xs"
    :class="clickable ? 'hover:opacity-80 focus-visible:outline focus-visible:outline-primary-500' : ''"
    :aria-label="clickable ? t('requestTiming.title') : undefined">
    <span class="text-gray-400 dark:text-gray-500">{{ t('usage.userUploadDuration') }}</span>
    <span data-testid="user-upload-duration" class="font-medium tabular-nums" :title="t('usage.userUploadDurationHint')">{{ formatRequestPhaseDuration(row.content_timing?.request_phases?.user_upload_ms) }}</span>
    <span class="text-gray-400 dark:text-gray-500">{{ t('usage.processingDuration') }}</span>
    <span data-testid="processing-duration" class="font-medium tabular-nums" :title="t('usage.processingDurationHint')">{{ formatRequestPhaseDuration(row.content_timing?.request_phases?.processing_ms) }}</span>
    <span class="text-gray-400 dark:text-gray-500">{{ t('usage.userFirstTokenWithUpload') }}</span>
    <span data-testid="first-token-duration" class="font-medium tabular-nums" :title="t('usage.requestFirstTokenHint')">{{ formatRequestPhaseDuration(firstTokenMs) }}</span>
    <span class="text-gray-400 dark:text-gray-500">{{ t('usage.latencyTps') }}</span>
    <span data-testid="latency-tps" class="font-medium tabular-nums" :title="t('usage.latencyTpsHint')">{{ formatUsageOutputTps(row) ?? '—' }}</span>
    <template v-if="showCacheRate">
      <span class="text-gray-400 dark:text-gray-500">{{ t('usage.latencyCacheRate') }}</span>
      <span data-testid="latency-cache-rate" class="font-medium tabular-nums text-emerald-600 dark:text-emerald-400" :title="cacheRateTitle">{{ formatUsageCacheRate(row) ?? '—' }}</span>
    </template>
  </component>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { UsageLog } from '@/types'
import { formatRequestPhaseDuration } from '@/utils/usageRequestTiming'
import { formatUsageOutputTps } from '@/utils/usageTps'
import { formatUsageCacheRate, usageCacheRateParts } from '@/utils/usageCacheRate'

const props = defineProps<{ row: UsageLog; showCacheRate: boolean; clickable?: boolean }>()
const { t } = useI18n()
const firstTokenMs = computed(() => props.row.queue_timing?.status === 'measured'
  ? props.row.queue_timing.server_first_token_ms : props.row.first_token_ms)
const cacheRateTitle = computed(() => {
  const parts = usageCacheRateParts(props.row)
  return parts ? t('usage.latencyCacheRateHint', { input: parts.input.toLocaleString(), read: parts.read.toLocaleString(), write: parts.write.toLocaleString() }) : undefined
})
</script>
