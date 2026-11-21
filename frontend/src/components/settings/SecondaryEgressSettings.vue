<template>
  <section v-if="rows.length || loadError" class="card">
    <header class="border-b border-gray-100 p-6 dark:border-dark-700">
      <h2 class="text-lg font-semibold">{{ t('admin.settings.secondaryEgress.title') }}</h2>
      <p class="mt-1 text-sm text-gray-500">{{ t('admin.settings.secondaryEgress.description') }}</p>
    </header>
    <div v-if="loadError" class="p-6" role="alert">
      {{ t('admin.settings.secondaryEgress.failed') }}
      <button type="button" class="btn btn-secondary" @click="load">{{ t('admin.settings.secondaryEgress.retry') }}</button>
    </div>
    <div v-for="row in rows" :key="row.node" class="space-y-4 p-6">
      <h3 class="font-medium">{{ t('admin.settings.secondaryEgress.node', { node: row.node }) }}</h3>
      <div class="grid gap-4 md:grid-cols-2">
        <label class="block">
          <span class="input-label">{{ t('admin.settings.secondaryEgress.processing') }}</span>
          <input v-model.trim="row.processing_proxy_url" type="url" class="input" placeholder="http://proxy:17981" spellcheck="false" />
          <span class="text-xs text-gray-500">{{ t('admin.settings.secondaryEgress.processingHint') }}</span>
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.settings.secondaryEgress.forward') }}</span>
          <input v-model.trim="row.forward_proxy_url" type="url" class="input" placeholder="http://proxy:17982" spellcheck="false" />
          <span class="text-xs text-gray-500">{{ t('admin.settings.secondaryEgress.forwardHint') }}</span>
        </label>
      </div>
      <button type="button" class="btn btn-primary btn-sm" :disabled="!!saving" @click="save(row)">{{ t('common.save') }}</button>
    </div>
  </section>
</template>
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores'
import { listSecondaryEgress, updateSecondaryEgress, type SecondaryEgressRow } from '@/api/admin/settings'
const { t } = useI18n()
const app = useAppStore()
const rows = ref<SecondaryEgressRow[]>([])
const loadError = ref(false)
const saving = ref('')
async function load() {
  try { rows.value = await listSecondaryEgress(); loadError.value = false }
  catch { loadError.value = true }
}
async function save(row: SecondaryEgressRow) {
  saving.value = row.node
  try {
    await updateSecondaryEgress(row.node, { processing_proxy_url: row.processing_proxy_url, forward_proxy_url: row.forward_proxy_url })
    app.showSuccess(t('admin.settings.secondaryEgress.saved'))
  } catch (err) {
    app.showError(err instanceof Error ? err.message : t('admin.settings.secondaryEgress.failed'))
  } finally { saving.value = '' }
}
onMounted(load)
</script>
