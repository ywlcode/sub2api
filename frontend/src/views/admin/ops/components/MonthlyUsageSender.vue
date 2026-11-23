<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { StorageSerializers, useSessionStorage } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { opsAPI, type EmailNotificationConfig, type MonthlyUsageJob } from '@/api/admin/ops'

type Report = EmailNotificationConfig['report']
type Attempt = {
  month: string
  requestId: string
  job: MonthlyUsageJob | null
  rejection?: 'conflict' | 'rejected'
}

const props = defineProps<{ saved: Report | null; draft: Report; recipientInput: string; saving: boolean }>()
const { t } = useI18n()
const admin = useAuthStore()
const scope = 'admin.ops.settings.manualMonthly'
const lastMonth = () => {
  const beijing = new Date(Date.now() + 8 * 3600_000)
  beijing.setUTCDate(1)
  beijing.setUTCMonth(beijing.getUTCMonth() - 1)
  return beijing.toISOString().slice(0, 7)
}
// Keep the request identity across dialog closes, navigation and reloads in this admin's tab.
const attempt = useSessionStorage<Attempt | null>(`ops:manual-monthly:${admin.user?.id}`, null, {
  serializer: StorageSerializers.object, flush: 'sync', writeDefaults: false
})
const pending = computed(() => !!attempt.value && !attempt.value.rejection &&
  (!attempt.value.job || attempt.value.job.status === 'running'))
const maxMonth = ref(lastMonth())
const month = ref(pending.value ? attempt.value!.month : maxMonth.value)
const job = computed(() => attempt.value?.job)
const reportFields = ['enabled', 'daily_summary_enabled', 'weekly_summary_enabled', 'monthly_usage_enabled',
  'error_digest_enabled', 'account_health_enabled', 'monthly_usage_department_attribute_id'] as const
const block = computed(() => {
  const saved = props.saved
  if (!saved) return props.saving ? '' : 'verifySettings'
  if (props.recipientInput.trim() || reportFields.some(key => saved[key] !== props.draft[key]) ||
      saved.recipients.length !== props.draft.recipients.length ||
      saved.recipients.some(email => !props.draft.recipients.includes(email))) return 'saveFirst'
  if (!saved.enabled || !saved.recipients.length) return 'enableFirst'
  if (!/^\d{4}-(0[1-9]|1[0-2])$/.test(month.value) || month.value < '0001-01' || month.value > maxMonth.value) return 'invalidMonth'
  return ''
})

const submitting = ref(false)
const polling = ref(false)
const pollFailed = ref(false)
const confirmResend = ref(false)
const retryPost = computed(() => attempt.value?.month === month.value && !job.value && !!attempt.value)
const resend = computed(() => attempt.value?.month === month.value && !!job.value && job.value.status !== 'running')
const disabled = computed(() => !!block.value || props.saving || submitting.value || job.value?.status === 'running')
const actionLabel = computed(() => {
  if (submitting.value) return 'submitting'
  if (job.value?.status === 'running') return 'inProgress'
  if (confirmResend.value) return 'confirm'
  if (resend.value) return 'resend'
  return retryPost.value ? 'retry' : 'send'
})
let timer: ReturnType<typeof setTimeout> | undefined
const controller = new AbortController()

// randomUUID needs HTTPS. Internal HTTP installations can still generate an RFC 4122 v4 UUID.
function requestId() {
  if (crypto.randomUUID) return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 15) | 64
  bytes[8] = (bytes[8] & 63) | 128
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
  return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join('-')
}

function record(result: MonthlyUsageJob, current: Attempt) {
  if (controller.signal.aborted || attempt.value?.requestId !== current.requestId) return
  if (!result?.id || result.month !== current.month || !['running', 'sent', 'failed', 'uncertain'].includes(result.status)) {
    throw new Error('Invalid monthly usage job')
  }
  attempt.value = { ...current, job: result }
  if (result.status === 'running') timer = setTimeout(poll, 2000)
}

async function poll() {
  const current = attempt.value
  if (polling.value || current?.job?.status !== 'running') return
  polling.value = true
  pollFailed.value = false
  try {
    record(await opsAPI.getMonthlyUsageJob(current.job.id, controller.signal), current)
  } catch {
    if (!controller.signal.aborted) pollFailed.value = true
  } finally {
    polling.value = false
  }
}

async function send() {
  maxMonth.value = lastMonth()
  if (disabled.value || (pending.value && attempt.value?.month !== month.value)) return
  if (resend.value && !confirmResend.value) {
    confirmResend.value = true
    return
  }
  const current: Attempt = {
    month: month.value, requestId: retryPost.value ? attempt.value!.requestId : requestId(), job: null
  }
  submitting.value = true
  confirmResend.value = false
  pollFailed.value = false
  attempt.value = current
  try {
    record(await opsAPI.sendMonthlyUsageReport(current.month, current.requestId, controller.signal), current)
  } catch (error) {
    if (controller.signal.aborted) return
    const status = (error as { status?: number })?.status
    if (status && status >= 400 && status < 500 && status !== 408) {
      attempt.value = { ...current, rejection: status === 409 ? 'conflict' : 'rejected' }
    }
    // A transport timeout may follow acceptance. Retry the same month and request ID.
  } finally {
    submitting.value = false
  }
}

watch([month, block], () => { confirmResend.value = false })
onMounted(() => { if (job.value?.status === 'running') void poll() })
onUnmounted(() => { clearTimeout(timer); controller.abort() })
</script>

<template>
  <section class="space-y-2 border-t border-gray-200 pt-4 dark:border-dark-600">
    <h5 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t(`${scope}.title`) }}</h5>
    <p v-if="saved?.recipients.length" class="break-words text-xs text-gray-600 dark:text-gray-300">
      {{ t(`${scope}.recipients`, { emails: saved.recipients.join(', ') }) }}
    </p>
    <div class="flex flex-wrap items-end gap-2">
      <label class="min-w-0 text-xs text-gray-600 dark:text-gray-300">
        {{ t(`${scope}.month`) }}
        <input v-model="month" type="month" class="input mt-1" :max="maxMonth" :disabled="pending || submitting" @focus="maxMonth = lastMonth()" />
      </label>
      <button type="button" class="btn btn-secondary" :disabled="disabled" @click="send">
        {{ t(`${scope}.${actionLabel}`) }}
      </button>
      <button v-if="confirmResend" type="button" class="btn btn-secondary" @click="confirmResend = false">{{ t('common.cancel') }}</button>
    </div>
    <p v-if="block" class="text-xs text-amber-700 dark:text-amber-300">{{ t(`${scope}.${block}`) }}</p>
    <p v-if="confirmResend" class="text-xs text-amber-700 dark:text-amber-300">{{ t(`${scope}.duplicate`) }}</p>
    <div v-if="attempt && !submitting" role="status" class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
      <p>{{ attempt.month }} · {{ t(`${scope}.${pollFailed ? 'pollUnknown' : job?.status ?? attempt.rejection ?? 'unknown'}`) }}</p>
      <p v-if="job" class="text-xs">{{ t(`${scope}.counts`, { total: job.total, accepted: job.accepted, failed: job.failed, uncertain: job.uncertain }) }}</p>
      <button v-if="pollFailed" type="button" class="btn btn-secondary" :disabled="polling" @click="poll">{{ t(`${scope}.check`) }}</button>
    </div>
  </section>
</template>
