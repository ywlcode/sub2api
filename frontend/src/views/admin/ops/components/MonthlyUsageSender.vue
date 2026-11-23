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
  expectedReportRevision?: string
  lookupOnly?: boolean
  rejection?: 'conflict' | 'rejected' | 'staleRevision' | 'revisionRequired' | 'idConflict' | 'rateLimited' | 'jobMissing'
}

const props = defineProps<{ saved: Report | null; draft: Report; recipientInput: string; saving: boolean; reportRevision?: string }>()
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
const storageKey = `ops:manual-monthly:${admin.user?.id}`
const attempt = useSessionStorage<Attempt | null>(storageKey, null, {
  serializer: StorageSerializers.object, flush: 'sync', writeDefaults: false
})
const resendMonths = useSessionStorage<string[]>(storageKey + ':resend-months', [], { flush: 'sync', writeDefaults: false })
function requireResendConfirmation(month: string) {
  if (!resendMonths.value.includes(month)) resendMonths.value.push(month)
}
// Older clients only stored the most recent job.
if (attempt.value?.job) requireResendConfirmation(attempt.value.month)
const pending = computed(() => !!attempt.value && !attempt.value.rejection &&
  (!attempt.value.job || attempt.value.job.status === 'running'))
const maxMonth = ref(lastMonth())
const month = ref(pending.value ? attempt.value!.month : maxMonth.value)
const job = computed(() => attempt.value?.job)
const reportFields = ['enabled', 'daily_summary_enabled', 'weekly_summary_enabled', 'monthly_usage_enabled',
  'error_digest_enabled', 'account_health_enabled', 'monthly_usage_department_attribute_id'] as const
const recovering = computed(() => pending.value && !job.value)
const lookupOnly = computed(() => recovering.value &&
  (attempt.value?.lookupOnly || !attempt.value?.expectedReportRevision || !props.reportRevision?.trim()))
const block = computed(() => {
  if (pending.value) return ''
  const saved = props.saved
  if (!saved) return props.saving ? '' : 'verifySettings'
  if (props.recipientInput.trim() || reportFields.some(key => saved[key] !== props.draft[key]) ||
      saved.recipients.length !== props.draft.recipients.length ||
      saved.recipients.some(email => !props.draft.recipients.includes(email))) return 'saveFirst'
  if (!props.reportRevision?.trim()) return 'missingRevision'
  if (!saved.enabled || !saved.recipients.length) return 'enableFirst'
  if (!/^\d{4}-(0[1-9]|1[0-2])$/.test(month.value) || month.value < '0001-01' || month.value > maxMonth.value) return 'invalidMonth'
  return ''
})

const submitting = ref(false)
const polling = ref(false)
const pollFailed = ref(false)
const confirmResend = ref(false)
const recoveryError = ref<'' | 'rateLimitedUnknown' | 'lookupUnknown'>('')
const statusLabel = computed(() => {
  if (pollFailed.value) return 'pollUnknown'
  return job.value?.status ?? attempt.value?.rejection ?? (recoveryError.value || (lookupOnly.value ? 'lookupNeeded' : 'unknown'))
})
const resend = computed(() => !pending.value && resendMonths.value.includes(month.value))
const disabled = computed(() => !!block.value || props.saving || submitting.value || job.value?.status === 'running' ||
  (pending.value && attempt.value?.month !== month.value))
const actionLabel = computed(() => {
  if (submitting.value) return 'submitting'
  if (job.value?.status === 'running') return 'inProgress'
  if (lookupOnly.value) return 'lookup'
  if (recovering.value) return 'retry'
  return resend.value ? 'resend' : 'send'
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
  if (!result || result.id !== current.requestId || result.month !== current.month ||
      !['running', 'sent', 'failed', 'uncertain'].includes(result.status)) {
    throw new Error('Invalid monthly usage job')
  }
  requireResendConfirmation(current.month)
  attempt.value = { ...current, job: result }
  if (result.status === 'running') timer = setTimeout(poll, 2000)
}

async function poll() {
  const current = attempt.value
  if (polling.value || current?.job?.status !== 'running') return
  polling.value = true
  pollFailed.value = false
  try {
    record(await opsAPI.getMonthlyUsageJob(current.requestId, controller.signal), current)
  } catch {
    if (!controller.signal.aborted) pollFailed.value = true
  } finally {
    polling.value = false
  }
}

function send() {
  maxMonth.value = lastMonth()
  if (disabled.value || confirmResend.value) return
  if (resend.value) {
    confirmResend.value = true
    return
  }
  void submit()
}

function confirm(event: MouseEvent) {
  if (!confirmResend.value || !resend.value || disabled.value || event.detail > 1) return
  void submit()
}

function preventRepeatedActivation(event: KeyboardEvent) {
  if (event.repeat && (event.key === 'Enter' || event.key === ' ')) event.preventDefault()
}

function rejectionFor(status: number, reason?: string): Attempt['rejection'] {
  if (status === 428) return 'revisionRequired'
  if (status === 429) return 'rateLimited'
  if (status === 409) {
    if (reason === 'MONTHLY_USAGE_REPORT_REVISION_CONFLICT') return 'staleRevision'
    if (reason === 'MONTHLY_USAGE_REPORT_ID_CONFLICT') return 'idConflict'
    if (!reason || reason === 'MONTHLY_USAGE_REPORT_BUSY') return 'conflict'
  }
  return 'rejected'
}

async function submit() {
  const previous = recovering.value ? attempt.value : null
  const lookup = lookupOnly.value
  const current: Attempt = previous ?? {
    month: month.value, requestId: requestId(), expectedReportRevision: props.reportRevision, job: null
  }
  submitting.value = true
  confirmResend.value = false
  pollFailed.value = false
  recoveryError.value = ''
  attempt.value = current
  try {
    const result = lookup
      ? await opsAPI.getMonthlyUsageJob(current.requestId, controller.signal)
      : await opsAPI.sendMonthlyUsageReport(current.month, current.requestId, controller.signal, current.expectedReportRevision)
    record(result, current)
  } catch (error) {
    if (controller.signal.aborted || attempt.value?.requestId !== current.requestId) return
    const { status, reason, message } = (error ?? {}) as { status?: number; reason?: string; message?: string }
    if (lookup) {
      const missingJob = reason === 'MONTHLY_USAGE_REPORT_JOB_NOT_FOUND' ||
        (!reason && message === 'Monthly usage report job not found')
      if (status === 404 && missingJob) {
        // The original POST might still be in flight. Require consent before starting another.
        requireResendConfirmation(current.month)
        attempt.value = { ...current, rejection: 'jobMissing' }
      } else {
        recoveryError.value = 'lookupUnknown'
      }
      return
    }
    if (!status || status < 400 || status >= 500 || status === 408) return
    if (previous) {
      // Rejection is for this retry. The first POST might still create a job.
      if (status === 429) recoveryError.value = 'rateLimitedUnknown'
      else attempt.value = { ...current, lookupOnly: true }
    } else {
      attempt.value = { ...current, rejection: rejectionFor(status, reason) }
    }
  } finally {
    submitting.value = false
  }
}

watch([month, block, () => props.reportRevision, () => props.saving, pending], () => { confirmResend.value = false })
onMounted(() => { if (job.value?.status === 'running') void poll() })
onUnmounted(() => { clearTimeout(timer); controller.abort() })
</script>

<template>
  <section class="space-y-2 border-t border-gray-200 pt-4 dark:border-dark-600">
    <h5 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t(`${scope}.title`) }}</h5>
    <p v-if="saved?.recipients.length" class="break-words text-xs text-gray-600 dark:text-gray-300">
      {{ t(`${scope}.recipients`, { emails: saved.recipients.join(', ') }) }}
    </p>
    <div class="flex flex-wrap items-end gap-2" @keydown="preventRepeatedActivation">
      <label class="min-w-0 text-xs text-gray-600 dark:text-gray-300">
        {{ t(`${scope}.month`) }}
        <input v-model="month" type="month" class="input mt-1" :max="maxMonth" :disabled="pending || submitting" @focus="maxMonth = lastMonth()" />
      </label>
      <button type="button" class="btn btn-secondary" :disabled="disabled || confirmResend" @click="send">
        {{ t(`${scope}.${actionLabel}`) }}
      </button>
      <button v-if="confirmResend" type="button" class="btn btn-secondary" @click="confirmResend = false">{{ t('common.cancel') }}</button>
      <button v-if="confirmResend" type="button" class="btn btn-primary" :disabled="disabled" @click="confirm">
        {{ t(`${scope}.confirm`) }}
      </button>
    </div>
    <p v-if="block" class="text-xs text-amber-700 dark:text-amber-300">{{ t(`${scope}.${block}`) }}</p>
    <p v-if="confirmResend" class="text-xs text-amber-700 dark:text-amber-300">{{ t(`${scope}.duplicate`, { month }) }}</p>
    <div v-if="attempt && !submitting" role="status" class="space-y-1 text-sm text-gray-700 dark:text-gray-300">
      <p>{{ attempt.month }} · {{ t(`${scope}.${statusLabel}`) }}</p>
      <p v-if="job" class="text-xs">{{ t(`${scope}.counts`, { total: job.total, accepted: job.accepted, failed: job.failed, uncertain: job.uncertain }) }}</p>
      <button v-if="pollFailed" type="button" class="btn btn-secondary" :disabled="polling" @click="poll">{{ t(`${scope}.check`) }}</button>
    </div>
  </section>
</template>
