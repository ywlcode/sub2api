<template>
  <div v-if="column === 'session'" class="min-w-[120px] space-y-1 text-xs">
    <div v-for="item in identifiers" :key="item.key" class="flex items-center gap-1.5">
      <span class="shrink-0 text-gray-400 dark:text-gray-500">{{ t(`usage.attribution.${item.key}`) }}</span>
      <button
        type="button"
        class="font-mono text-primary-600 hover:underline dark:text-primary-400"
        :title="`${item.value}\n${t('keys.copyToClipboard')}`"
        :aria-label="`${t('keys.copyToClipboard')}: ${item.value}`"
        @click="copyID(item.value)"
      >{{ shortID(item.value) }}</button>
    </div>
    <span v-if="!identifiers.length" class="text-gray-400 dark:text-gray-500">{{ t(row.client_context?.identity_status ? `usage.attribution.identity_${row.client_context.identity_status}` : 'usage.attribution.notProvided') }}</span>
  </div>
  <div v-else class="min-w-[110px] space-y-1.5" :title="sourceHint">
    <div class="text-xs font-medium text-gray-700 dark:text-gray-300">{{ clientName || t('usage.attribution.unknownClient') }}</div>
    <div class="flex flex-wrap gap-1">
      <span
        v-for="kind in kinds" :key="kind"
        class="inline-flex rounded px-1.5 py-0.5 text-xs font-medium"
        :class="kind === 'subagent' ? 'bg-violet-100 text-violet-700 dark:bg-violet-900/30 dark:text-violet-300' : kind === 'compaction' ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-400'"
      >{{ t(`usage.attribution.${kind}`) }}</span>
    </div>
    <div v-if="row.client_context?.agent_type" class="max-w-[160px] truncate text-xs text-gray-500 dark:text-gray-400">{{ row.client_context.agent_type }}</div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useClipboard } from '@vueuse/core'
import { useAppStore } from '@/stores/app'
import { usageClientName, usageContextIDs, usageRequestSourceText, usageSourceKinds, type UsageContextRow } from '@/utils/usageClientContext'

const props = defineProps<{ row: UsageContextRow; column: 'session' | 'source' }>()
const { t } = useI18n()
const appStore = useAppStore()
const { copy } = useClipboard({ legacy: true })
const identifiers = computed(() => usageContextIDs(props.row))
const clientName = computed(() => usageClientName(props.row))
const kinds = computed(() => usageSourceKinds(props.row.client_context))
const sourceHint = computed(() => [
  usageRequestSourceText(props.row, t),
  props.row.client_context?.turn_id ? `${t('usage.attribution.turn')}: ${props.row.client_context.turn_id}` : '',
  t('usage.attribution.hint'),
].filter(Boolean).join('\n'))
const shortID = (value: string) => value.length > 18 ? `${value.slice(0, 10)}…${value.slice(-6)}` : value
async function copyID(value: string) {
  try {
    await copy(value)
    appStore.showSuccess(t('keys.copied'))
  } catch {
    appStore.showError(t('common.copyFailed'))
  }
}
</script>
