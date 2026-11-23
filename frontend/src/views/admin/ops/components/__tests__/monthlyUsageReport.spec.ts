import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import OpsSettingsDialog from '../OpsSettingsDialog.vue'
import OpsEmailNotificationCard from '../OpsEmailNotificationCard.vue'
import MonthlyUsageDepartmentField from '../MonthlyUsageDepartmentField.vue'
import MonthlyUsageSender from '../MonthlyUsageSender.vue'
import Toggle from '@/components/common/Toggle.vue'

const { get, put, post } = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, put, post } }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 17 } }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, params?: { id?: number }) => params?.id ? key + ' #' + params.id : key }) }))
enableAutoUnmount(afterEach)

const configPath = '/admin/ops/email-notification/config'
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
      return { data: path === configPath ? structuredClone({ alert: legacyConfig.alert, report }) : settings[path] }
    })
    put.mockImplementation(async (path: string, payload) => {
      const data = JSON.parse(JSON.stringify(payload))
      if (path === configPath) {
        report = { ...report, ...data.report }
        return { data: { ...data, report: structuredClone(report) } }
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
    expect(put).toHaveBeenCalledWith(configPath, {
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
      { month, request_id: expect.any(String) }, { signal: expect.any(AbortSignal) })
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
    expect(put.mock.calls.find(([path]) => path === configPath)?.[1].report).not.toHaveProperty('monthly_usage_department_attribute_id')
    expect(report.monthly_usage_department_attribute_id).toBe(41)
    wrapper.unmount()

    const reopened = await open()
    await reopened.getComponent(MonthlyUsageDepartmentField).get('select').setValue('0')
    expect(reopened.getComponent(MonthlyUsageSender).get('button').element.disabled).toBe(true)
    await reopened.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(report.monthly_usage_department_attribute_id).toBe(0)
  })

  it('keeps the manual guard when an older API ignores a custom field on save', async () => {
    const wrapper = await open()
    await wrapper.getComponent(MonthlyUsageDepartmentField).get('select').setValue('7')
    put.mockImplementation(async (path, data) => ({ data: path === configPath ? legacyConfig : data }))
    const err = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      await wrapper.get('button.btn-primary').trigger('click')
      await flushPromises()
      const sender = wrapper.getComponent(MonthlyUsageSender)
      expect(sender.text()).toContain('verifySettings')
      expect(sender.get('button').element.disabled).toBe(true)
    } finally {
      err.mockRestore()
    }
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

  it('blocks manual send after an unconfirmed settings save, even if edits are reverted', async () => {
    const w = await open()
    const err = vi.spyOn(console, 'error').mockImplementation(() => {})
    try {
      const input = w.findAll('input[type="email"]').at(-1)!
      await input.setValue('additional@example.com')
      await input.trigger('keydown.enter')
      put.mockImplementation(async (path: string, data: unknown) => {
        if (path === configPath) throw new Error('settings update timed out')
        return { data }
      })
      await w.get('button.btn-primary').trigger('click')
      await flushPromises()
      const chip = w.findAll('span').find(el => el.text().includes('additional@example.com') && el.find('button').exists())!
      await chip.get('button').trigger('click')
      const sender = w.getComponent(MonthlyUsageSender)
      expect(sender.text()).toContain('verifySettings')
      expect(sender.get('button').element.disabled).toBe(true)
    } finally {
      err.mockRestore()
    }
  })

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
      expect(put).toHaveBeenCalledWith(configPath, {
        alert: legacyConfig.alert, report: { ...legacyConfig.report, recipients: [], monthly_usage_enabled: true, enabled: false }
      })
    }
  })
})
