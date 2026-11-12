<script setup lang="ts">
import { useId } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'

const enabled = defineModel<boolean>({ default: false })
const { t } = useI18n()
const hintId = useId()
</script>

<template>
  <div class="flex items-center justify-between gap-4">
    <div>
      <span class="input-label mb-0">{{ t('admin.accounts.secondaryProcessing.title') }}</span>
      <p :id="hintId" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.secondaryProcessing.hint') }}
      </p>
    </div>
    <Toggle
      v-model="enabled"
      data-testid="secondary-processing-toggle"
      :aria-label="t('admin.accounts.secondaryProcessing.title')"
      :aria-describedby="hintId"
    />
  </div>
</template>
