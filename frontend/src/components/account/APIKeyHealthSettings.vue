<template>
  <div class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" data-testid="api-key-health-settings">
    <div class="flex items-center justify-between gap-4">
      <div>
        <label class="input-label mb-0">{{ t('admin.accounts.healthBreaker.title') }}</label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.healthBreaker.hint') }}</p>
      </div>
      <Toggle :model-value="modelValue.enabled" :aria-label="t('admin.accounts.healthBreaker.title')"
        @update:model-value="update({ enabled: $event })" />
    </div>
    <div v-if="modelValue.enabled" class="space-y-3">
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.healthBreaker.windowSize') }}</span>
          <input :value="modelValue.window_size" type="number" min="1" max="10000" step="1" required
            class="input" data-testid="api-key-health-window"
            @input="update({ window_size: Number(($event.target as HTMLInputElement).value) })" />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.accounts.healthBreaker.failureThreshold') }}</span>
          <input :value="modelValue.failure_threshold" type="number" min="1" :max="modelValue.window_size" step="1" required
            class="input" data-testid="api-key-health-threshold"
            @input="update({ failure_threshold: Number(($event.target as HTMLInputElement).value) })" />
        </label>
      </div>
      <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.healthBreaker.policy') }}</p>
      <p v-if="!validAPIKeyHealthConfig(modelValue)" class="text-xs text-red-500" role="alert">
        {{ t('admin.accounts.healthBreaker.invalid') }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { validAPIKeyHealthConfig, type APIKeyHealthConfig } from './apiKeyHealth'

const props = defineProps<{ modelValue: APIKeyHealthConfig }>()
const emit = defineEmits<{ 'update:modelValue': [value: APIKeyHealthConfig] }>()
const { t } = useI18n()
const update = (patch: Partial<APIKeyHealthConfig>) => emit('update:modelValue', { ...props.modelValue, ...patch })
</script>
