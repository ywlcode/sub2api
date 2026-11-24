<template>
  <section class="card mt-4 space-y-4 p-5" data-testid="moderation-records" :aria-label="t('securityRecords.moderation.title')">
    <div class="flex items-center justify-between gap-3">
      <h2 class="text-lg font-semibold">{{ t('securityRecords.moderation.title') }}</h2>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadPage(requestedPage)">
        {{ t('common.refresh') }}
      </button>
    </div>
    <p v-if="loading" role="status">{{ t('common.loading') }}</p>
    <p v-else-if="failed" role="alert" class="text-red-600">{{ t('securityRecords.moderation.loadFailed') }}</p>
    <template v-else-if="records">
      <div class="space-y-1">
        <p v-if="records.account_banned" class="font-medium text-red-600">{{ t('securityRecords.moderation.allPlatformsBlocked') }}</p>
        <p v-else-if="records.openai_banned" class="font-medium text-red-600">{{ t('securityRecords.moderation.openaiBanned') }}</p>
        <p v-else class="text-gray-500">{{ t('securityRecords.moderation.noBan') }}</p>
      </div>
      <dl class="grid gap-3 rounded-lg bg-gray-50 p-3 text-sm sm:grid-cols-3 dark:bg-dark-900">
        <div>
          <dt class="text-gray-500">{{ t('securityRecords.moderation.currentCount') }}</dt>
          <dd class="font-medium tabular-nums">{{ records.current_count }} / {{ records.threshold }}</dd>
        </div>
        <div>
          <dt class="text-gray-500">{{ t('securityRecords.moderation.autoBan') }}</dt>
          <dd class="font-medium">{{ t(records.auto_ban_enabled ? 'common.enabled' : 'common.disabled') }}</dd>
        </div>
        <div>
          <dt class="text-gray-500">{{ t('securityRecords.moderation.totalBans') }}</dt>
          <dd class="font-medium tabular-nums">{{ records.recorded_ban_count }}</dd>
        </div>
      </dl>
      <h3 class="font-medium">{{ t('securityRecords.moderation.history') }}</h3>
      <p v-if="!records.items.length" class="text-sm text-gray-500">{{ t('securityRecords.moderation.empty') }}</p>
      <article v-for="item in records.items" :key="item.id" class="space-y-2 rounded-lg border p-3 text-sm dark:border-dark-600">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div class="min-w-0">
            <p class="text-gray-500">#{{ item.id }} · <time :datetime="item.created_at">{{ displayTime(item.created_at) }}</time></p>
            <p class="break-words font-medium">{{ actionLabel(item.action) }}<span v-if="item.model"> · {{ item.model }}</span></p>
          </div>
          <button type="button" class="btn btn-secondary btn-sm" :aria-expanded="expanded === item.id" @click="expanded = expanded === item.id ? null : item.id">
            {{ t(expanded === item.id ? 'securityRecords.moderation.hideDetails' : 'securityRecords.moderation.showDetails') }}
          </button>
        </div>
        <div v-if="expanded === item.id" class="space-y-3 border-t pt-3 dark:border-dark-600">
          <dl class="grid gap-3 sm:grid-cols-2">
            <div>
              <dt class="text-gray-500">{{ t('securityRecords.moderation.category') }}</dt>
              <dd class="break-words">{{ item.category || '—' }}</dd>
            </div>
            <div>
              <dt class="text-gray-500">{{ t('securityRecords.moderation.historicalCount') }}</dt>
              <dd>{{ item.violation_count }}</dd>
            </div>
            <div>
              <dt class="text-gray-500">{{ t('securityRecords.moderation.banOutcome') }}</dt>
              <dd>{{ banOutcome(item.ban_applied) }}</dd>
            </div>
            <div v-if="item.ban_applied && item.ban_scope">
              <dt class="text-gray-500">{{ t('securityRecords.moderation.banScope') }}</dt>
              <dd>{{ t(item.ban_scope === 'all' ? 'securityRecords.moderation.allPlatforms' : 'securityRecords.moderation.openaiOnly') }}</dd>
            </div>
            <div v-if="item.ban_expires_at">
              <dt class="text-gray-500">{{ t('securityRecords.moderation.scheduledRelease') }}</dt>
              <dd><time :datetime="item.ban_expires_at">{{ displayTime(item.ban_expires_at) }}</time></dd>
            </div>
          </dl>
          <div>
            <p class="text-gray-500">{{ t('securityRecords.moderation.excerpt') }}</p>
            <pre v-if="item.input_excerpt?.trim()" class="mt-1 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-3 dark:bg-dark-900">{{ item.input_excerpt }}</pre>
            <p v-else>{{ t('securityRecords.moderation.noExcerpt') }}</p>
          </div>
        </div>
      </article>
      <nav v-if="records.total > records.page_size || records.page > 1" class="flex flex-wrap items-center gap-3" :aria-label="t('securityRecords.moderation.history')">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="records.page <= 1" @click="loadPage(records.page - 1)">{{ t('securityRecords.previous') }}</button>
        <span class="text-sm">{{ t('pagination.pageOf', { page: records.page, total: Math.ceil(records.total / records.page_size) }) }}</span>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="records.page * records.page_size >= records.total" @click="loadPage(records.page + 1)">{{ t('securityRecords.next') }}</button>
      </nav>
    </template>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { listMyModerationRecords, type ModerationRecordsPage } from './api'

const { t, te, locale } = useI18n()
const records = ref<ModerationRecordsPage | null>(null)
const expanded = ref<number | null>(null)
const loading = ref(false)
const failed = ref(false)
const requestedPage = ref(1)

function displayTime(value: string) {
  return new Date(value).toLocaleString(locale.value)
}

function actionLabel(action: string) {
  const key = `securityRecords.moderation.actions.${action}`
  return te(key) ? t(key) : action
}

function banOutcome(applied: boolean | null) {
  if (applied === true) return t('securityRecords.moderation.banApplied')
  if (applied === false) return t('securityRecords.moderation.banNotApplied')
  return t('securityRecords.moderation.unknownOutcome')
}

async function loadPage(page: number) {
  if (loading.value) return
  loading.value = true
  failed.value = false
  expanded.value = null
  requestedPage.value = page
  try {
    records.value = await listMyModerationRecords(page)
    requestedPage.value = records.value.page
  } catch {
    records.value = null
    failed.value = true
  } finally {
    loading.value = false
  }
}

onMounted(() => loadPage(1))
</script>
