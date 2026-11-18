<template>
  <div class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-600" data-testid="transport-cooldown-settings">
    <div class="flex items-center justify-between gap-4">
      <div>
        <label class="input-label mb-0">{{ t('admin.accounts.transportCooldown.title') }}</label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.transportCooldown.hint') }}</p>
      </div>
      <Toggle :model-value="modelValue.enabled" :aria-label="t('admin.accounts.transportCooldown.title')"
        @update:model-value="update({ enabled: $event })" />
    </div>
    <label v-if="modelValue.enabled" class="block">
      <span class="input-label">{{ t('admin.accounts.transportCooldown.duration') }}</span>
      <input :value="modelValue.duration_minutes" type="number" min="1" max="1440" step="1" required
        class="input" data-testid="transport-cooldown-duration"
        @input="update({ duration_minutes: Number(($event.target as HTMLInputElement).value) })" />
    </label>
    <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.transportCooldown.independent') }}</p>
    <p v-if="!validTransportCooldownConfig(modelValue)" class="text-xs text-red-500" role="alert">
      {{ t('admin.accounts.transportCooldown.invalid') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { validTransportCooldownConfig, type TransportCooldownConfig } from './transportCooldown'

const props = defineProps<{ modelValue: TransportCooldownConfig }>()
const emit = defineEmits<{ 'update:modelValue': [value: TransportCooldownConfig] }>()
const { t } = useI18n()
const update = (patch: Partial<TransportCooldownConfig>) => emit('update:modelValue', { ...props.modelValue, ...patch })
</script>
