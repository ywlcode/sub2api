import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import OpsSettingsDialog from '../OpsSettingsDialog.vue'
import OpsEmailNotificationCard from '../OpsEmailNotificationCard.vue'
import MonthlyUsageDepartmentField from '../MonthlyUsageDepartmentField.vue'
import MonthlyUsageSender from '../MonthlyUsageSender.vue'
import { opsAPI } from '@/api/admin/ops'
import Toggle from '@/components/common/Toggle.vue'

const { get, put, post } = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, put, post } }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 17 } }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, params?: { id?: number }) => params?.id ? key + ' #' + params.id : key }) }))
enableAutoUnmount(afterEach)

const configPath = '/admin/ops/email-notification/config'
const conditionalPath = configPath + '/conditional'
const legacyConfig = {
  alert: {
    enabled: false, recipients: ['alerts@example.com'], min_severity: '',
    rate_limit_per_hour: 0, batching_window_seconds: 0, include_resolved_alerts: false
  },
  report: {
    enabled: true, recipients: ['reports@example.com'],
    daily_summary_enabled: true, daily_summary_schedule: '30 8 * * *',
    weekly_summary_enabled: false, weekly_summary_schedule: '15 10 * * 5',
    error_digest_enabled: false, error_digest_schedule: '', error_digest_min_count: 0,
    account_health_enabled: false, account_health_schedule: '', account_health_error_rate_threshold: 10
  }
}

const stubs = {
  BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
  Toggle, MonthlyUsageSender, MonthlyUsageDepartmentField
}
async function openSettings() {
  const wrapper = shallowMount(OpsSettingsDialog, { props: { show: false }, global: { stubs } })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}
async function openEmailCard() {
  const wrapper = shallowMount(OpsEmailNotificationCard, { global: { stubs } })
  await flushPromises()
  await wrapper.get('button.btn-secondary').trigger('click')
  await flushPromises()
  return wrapper
}

describe.each([
  { editor: 'ops settings', open: openSettings, control: '#ops-monthly-usage' },
  { editor: 'email notifications', open: openEmailCard, control: '[aria-describedby="ops-email-monthly-usage-hint"]' }
])('monthly usage report in $editor', ({ open, control }) => {
  let revision: string | undefined
  let report: typeof legacyConfig.report & { monthly_usage_enabled?: boolean; monthly_usage_department_attribute_id?: number }
  const attributes = [
    { id: 7, name: 'Department', type: 'text', enabled: true },
    { id: 9, name: 'Department', type: 'select', enabled: true },
    { id: 12, name: 'Division', type: 'textarea', enabled: true },
    { id: 13, name: 'Disabled', type: 'text', enabled: false },
    { id: 14, name: 'Headcount', type: 'number', enabled: true },
    { id: 15, name: 'Tags', type: 'multi_select', enabled: true }
  ]
  const listAttributes = vi.fn()
  beforeEach(() => {
    vi.resetAllMocks()
    sessionStorage.clear()
    revision = 'initial'
    report = structuredClone(legacyConfig.report)
    const settings: Record<string, unknown> = {
      '/admin/ops/runtime/alert': { evaluation_interval_seconds: 30 },
      '/admin/ops/advanced-settings': {
        data_retention: { cleanup_enabled: false }, aggregation: { aggregation_enabled: false },
        ignore_count_tokens_errors: false, ignore_context_canceled: false,
        ignore_no_available_accounts: false, ignore_insufficient_balance_errors: false,
        auto_refresh_enabled: false, display_alert_events: false, display_openai_token_stats: false
      },
      '/admin/ops/settings/metric-thresholds': {}
    }
    listAttributes.mockResolvedValue(attributes)
    get.mockImplementation(async (path: string) => {
      if (path === '/admin/user-attributes') return { data: await listAttributes() }
      return { data: path === configPath ? structuredClone({ alert: legacyConfig.alert, report, report_revision: revision }) : settings[path] }
    })
    put.mockImplementation(async (path: string, payload) => {
      const data = JSON.parse(JSON.stringify(payload))
      if (path === configPath) throw new Error('Unsafe unconditional PUT')
      if (path === conditionalPath) {
        if (data.expected_report_revision !== revision) {
          throw { status: 409, reason: 'MONTHLY_USAGE_REPORT_REVISION_CONFLICT' }
        }
        revision = 'saved'
        report = { ...report, ...data.report }
        return { data: { alert: data.alert, report: structuredClone(report), report_revision: revision } }
      }
      return { data }
    })
  })

  it.each([
    { initial: undefined, toggle: false, expected: false },
    { initial: undefined, toggle: true, expected: true },
    { initial: true, toggle: true, expected: false }
  ])('loads $initial and saves $expected (toggle=$toggle) with existing report settings', async ({ initial, toggle, expected }) => {
    if (initial !== undefined) report.monthly_usage_enabled = initial
    const wrapper = await open()
    expect(wrapper.getComponent(MonthlyUsageDepartmentField).get('select').element.value).toBe('0')
    const monthly = wrapper.get(control)
    const checked = () => monthly.element instanceof HTMLInputElement
      ? monthly.element.checked
      : monthly.attributes('aria-checked') === 'true'
    expect(checked()).toBe(initial ?? false)
    if (toggle) {
      if (monthly.element instanceof HTMLInputElement) await monthly.setValue(expected)
      else await monthly.trigger('click')
    }
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(put).toHaveBeenCalledWith(conditionalPath, {
      expected_report_revision: 'initial',
      alert: legacyConfig.alert,
      report: { ...legacyConfig.report, monthly_usage_enabled: expected }
    })
  })

  it('selects an eligible field by name and ID, blocks unsaved sends and reopens the saved choice', async () => {
    report.monthly_usage_department_attribute_id = 7
    const wrapper = await open()
    const select = wrapper.getComponent(MonthlyUsageDepartmentField).get('select')
    const send = wrapper.getComponent(MonthlyUsageSender).get('button')
    expect(get).toHaveBeenCalledWith('/admin/user-attributes', { params: { enabled: true } })
    expect(select.element.value).toBe('7')
    expect(select.findAll('option').map(option => option.text())).toEqual([
      'admin.ops.settings.monthlyDepartment.automatic', 'Department (#7)', 'Department (#9)', 'Division (#12)'
    ])
    await select.setValue('9')
    expect(send.element.disabled).toBe(true)
    await send.trigger('click')
    expect(post).not.toHaveBeenCalled()
    await select.setValue('7')
    expect(send.element.disabled).toBe(false)
    await select.setValue('9')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(report.monthly_usage_department_attribute_id).toBe(9)
    wrapper.unmount()

    const reopened = await open()
    expect(reopened.getComponent(MonthlyUsageDepartmentField).get('select').element.value).toBe('9')
    const sender = reopened.getComponent(MonthlyUsageSender)
    const month = sender.get('input').element.value
    post.mockResolvedValue({ data: { id: 'job', month, status: 'sent', total: 1, accepted: 1, failed: 0, uncertain: 0 } })
    await sender.get('button').trigger('click')
    expect(post).toHaveBeenCalledWith('/admin/ops/email-notification/monthly-usage/send',
      { month, request_id: expect.any(String), expected_report_revision: 'saved' }, { signal: expect.any(AbortSignal) })
  })

  it.each(['deleted', 'load failed'])('preserves the field when %s until Automatic is explicitly saved', async mode => {
    report.monthly_usage_department_attribute_id = 41
    if (mode === 'load failed') listAttributes.mockRejectedValue(new Error('offline'))
    const wrapper = await open()
    const field = wrapper.getComponent(MonthlyUsageDepartmentField)
    const select = field.get('select')
    const selected = select.element.selectedOptions[0]
    expect(selected.value).toBe('41')
    expect(selected.textContent).toContain(mode === 'deleted' ? 'unavailable #41' : 'current #41')
    expect(field.find('[role="status"]').exists()).toBe(mode === 'load failed')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(put.mock.calls.find(([path]) => path === conditionalPath)?.[1].report).not.toHaveProperty('monthly_usage_department_attribute_id')
    expect(report.monthly_usage_department_attribute_id).toBe(41)
    wrapper.unmount()

    const reopened = await open()
    await reopened.getComponent(MonthlyUsageDepartmentField).get('select').setValue('0')
    expect(reopened.getComponent(MonthlyUsageSender).get('button').element.disabled).toBe(true)
    await reopened.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(report.monthly_usage_department_attribute_id).toBe(0)
  })

  it('shows the upgrade requirement and keeps unversioned email settings read-only', async () => {
    revision = undefined
    const wrapper = await open()
    expect(wrapper.text()).toContain('emailConfigUpgradeRequired')
    expect(wrapper.get(control).element.matches(':disabled')).toBe(true)
    const sender = wrapper.getComponent(MonthlyUsageSender)
    expect(sender.props('reportRevision')).toBeUndefined()
    expect(sender.get('button').element.disabled).toBe(true)
    await sender.get('button').trigger('click')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(post).not.toHaveBeenCalled()
    expect(put.mock.calls.some(([path]) => path === configPath || path === conditionalPath)).toBe(false)
    if (open === openSettings) expect(put).toHaveBeenCalledWith('/admin/ops/settings/metric-thresholds', expect.anything())
  })

  it('compares recipient and switch edits against an independent saved report', async () => {
    const wrapper = await open()
    const sender = wrapper.getComponent(MonthlyUsageSender)
    const button = sender.get('button')
    expect(button.attributes('disabled')).toBeUndefined()
    const input = wrapper.findAll('input[type="email"]').at(-1)!
    await input.setValue('second@example.com')
    expect(button.attributes('disabled')).toBeDefined()
    await input.trigger('keydown.enter')
    expect(sender.text()).toContain('saveFirst')
    expect(sender.props('saved').recipients).toEqual(['reports@example.com'])
    const chip = wrapper.findAll('span').find(el => el.text().includes('second@example.com') && el.find('button').exists())!
    await chip.get('button').trigger('click')
    expect(button.attributes('disabled')).toBeUndefined()
    const autoMonthly = wrapper.get(control)
    if (autoMonthly.element instanceof HTMLInputElement) await autoMonthly.setValue(true)
    else await autoMonthly.trigger('click')
    expect(sender.text()).toContain('saveFirst')
  })

  it('can look up a legacy pending request by ID while email settings are read-only', async () => {
    revision = undefined
    const month = '2025-02'
    const requestId = '2a37d66c-5e5e-429a-8f4b-6a63bbef9137'
    sessionStorage.setItem('ops:manual-monthly:17', JSON.stringify({ month, requestId, job: null }))
    const wrapper = await open()
    get.mockResolvedValueOnce({ data: { id: requestId, month, status: 'sent', total: 1, accepted: 1, failed: 0, uncertain: 0 } })
    const sender = wrapper.getComponent(MonthlyUsageSender)
    await sender.get('button').trigger('click')
    await flushPromises()
    expect(get).toHaveBeenCalledWith('/admin/ops/email-notification/monthly-usage/jobs/' + requestId, { signal: expect.any(AbortSignal) })
    expect(post).not.toHaveBeenCalled()
    expect(sender.get('[role="status"]').text()).toContain('sent')
  })

  it.each(['conflict', 'timeout', 'missing revision', 'missing conditional route', 'ignored field'])(
    'requires a fresh config and draft before retrying a save after %s', async mode => {
      const wrapper = await open()
      const input = wrapper.findAll('input[type="email"]').at(-1)!
      await input.setValue('draft@example.com')
      await input.trigger('keydown.enter')
      await wrapper.getComponent(MonthlyUsageDepartmentField).get('select').setValue('7')

      // Another admin changed the recipients after this editor loaded its draft.
      revision = 'newer'
      report.recipients = ['current@example.com']
      const defaultPut = put.getMockImplementation()!
      put.mockImplementation(async (path, payload) => {
        if (path === conditionalPath) {
          if (mode === 'timeout') throw new Error('timeout')
          if (mode === 'missing conditional route') throw { status: 404 }
          if (mode === 'missing revision') return { data: { ...legacyConfig, report: payload.report } }
          if (mode === 'ignored field') return { data: { ...legacyConfig, report_revision: revision } }
        }
        return defaultPut(path, payload)
      })
      const err = vi.spyOn(console, 'error').mockImplementation(() => {})
      try {
        const save = wrapper.get('button.btn-primary')
        await save.trigger('click')
        await flushPromises()
        expect(wrapper.text()).toContain('emailConfigReloadRequired')
        expect(save.attributes('disabled')).toBeDefined()
        await save.trigger('click')
        await flushPromises()
        expect(put.mock.calls.filter(([path]) => path === conditionalPath)).toHaveLength(1)
        expect(put).not.toHaveBeenCalledWith(configPath, expect.anything())
        expect(report.recipients).toEqual(['current@example.com'])
        const sender = wrapper.findComponent(MonthlyUsageSender)
        if (sender.exists()) expect(sender.get('button').element.disabled).toBe(true)

        const refresh = wrapper.findAll('button').find(button => button.text() === 'common.refresh' && !button.element.disabled)!
        await refresh.trigger('click')
        await flushPromises()
        const reloaded = wrapper.getComponent(MonthlyUsageSender)
        expect(reloaded.props('reportRevision')).toBe('newer')
        expect(reloaded.props('draft').recipients).toEqual(['current@example.com'])
        expect(reloaded.props('draft').monthly_usage_department_attribute_id).toBe(0)

        put.mockImplementation(defaultPut)
        await wrapper.get('button.btn-primary').trigger('click')
        await flushPromises()
        expect(put).toHaveBeenCalledWith(conditionalPath, expect.objectContaining({
          expected_report_revision: 'newer',
          report: expect.objectContaining({ recipients: ['current@example.com'] })
        }))
      } finally {
        err.mockRestore()
      }
    }
  )

  it('hides the monthly option when reports are disabled and preserves its saved preference', async () => {
    report.enabled = false
    report.monthly_usage_department_attribute_id = 7
    report.monthly_usage_enabled = true
    const wrapper = await open()
    expect(wrapper.find(control).exists()).toBe(false)
    expect(listAttributes).not.toHaveBeenCalled()
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(report.monthly_usage_department_attribute_id).toBe(7)
    expect(report.monthly_usage_enabled).toBe(true)
  })

  it('uses the shared report recipient requirement when monthly usage is enabled', async () => {
    report.recipients = []
    report.monthly_usage_enabled = true
    const wrapper = await open()
    const save = wrapper.get('button.btn-primary')
    if (open === openEmailCard) {
      expect(save.attributes('disabled')).toBeDefined()
    } else {
      await save.trigger('click')
      await flushPromises()
      expect(put).toHaveBeenCalledWith(conditionalPath, {
        expected_report_revision: 'initial',
        alert: legacyConfig.alert, report: { ...legacyConfig.report, recipients: [], monthly_usage_enabled: true, enabled: false }
      })
    }
  })
})

it('includes the version field even when the manual send API receives no version', async () => {
  post.mockResolvedValue({ data: {} })
  const signal = new AbortController().signal
  await opsAPI.sendMonthlyUsageReport('2025-02', 'test-id', signal)
  expect(post).toHaveBeenCalledWith('/admin/ops/email-notification/monthly-usage/send', {
    month: '2025-02', request_id: 'test-id', expected_report_revision: ''
  }, { signal })
})
