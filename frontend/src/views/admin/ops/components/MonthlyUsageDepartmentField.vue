<script setup lang="ts">
import { computed, onMounted, ref, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import { listEnabledDefinitions } from '@/api/admin/userAttributes'
import type { UserAttributeDefinition } from '@/types'

const departmentId = defineModel<number>({ required: true })
const { t } = useI18n()
const hintId = useId()
const scope = 'admin.ops.settings.monthlyDepartment'
const fields = ref<UserAttributeDefinition[]>([])
const status = ref<'loading' | 'ready' | 'failed'>('loading')
const missing = computed(() => departmentId.value > 0 && !fields.value.some(field => field.id === departmentId.value))

async function loadFields() {
  status.value = 'loading'
  try {
    const definitions = await listEnabledDefinitions()
    fields.value = definitions.filter(field => field.enabled && ['text', 'textarea', 'select'].includes(field.type))
    status.value = 'ready'
  } catch {
    status.value = 'failed'
  }
}

onMounted(loadFields)
</script>

<template>
  <div>
    <label class="block">
      <span class="input-label">{{ t(`${scope}.label`) }}</span>
      <select v-model="departmentId" class="input" :aria-describedby="hintId">
        <option :value="0">{{ t(`${scope}.automatic`) }}</option>
        <option v-if="missing" :value="departmentId" disabled>
          {{ t(status === 'ready' ? `${scope}.unavailable` : `${scope}.current`, { id: departmentId }) }}
        </option>
        <option v-for="field in fields" :key="field.id" :value="field.id">{{ field.name }} (#{{ field.id }})</option>
      </select>
    </label>
    <p :id="hintId" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(`${scope}.hint`) }}</p>
    <p v-if="status === 'failed'" role="status" class="mt-1 text-xs text-amber-700 dark:text-amber-400">
      {{ t(`${scope}.loadFailed`) }}
      <button type="button" class="text-primary-600 underline dark:text-primary-400" @click="loadFields">{{ t('common.refresh') }}</button>
    </p>
  </div>
</template>
