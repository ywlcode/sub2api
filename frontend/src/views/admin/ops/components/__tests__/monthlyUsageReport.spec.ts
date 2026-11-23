import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import OpsSettingsDialog from '../OpsSettingsDialog.vue'
import OpsEmailNotificationCard from '../OpsEmailNotificationCard.vue'
import MonthlyUsageSender from '../MonthlyUsageSender.vue'
import Toggle from '@/components/common/Toggle.vue'

const { get, put } = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, put } }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 17 } }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
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
  Toggle, MonthlyUsageSender
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
  return wrapper
}

describe.each([
  { editor: 'ops settings', open: openSettings, control: '#ops-monthly-usage' },
  { editor: 'email notifications', open: openEmailCard, control: '[aria-describedby="ops-email-monthly-usage-hint"]' }
])('monthly usage report in $editor', ({ open, control }) => {
  let report: typeof legacyConfig.report & { monthly_usage_enabled?: boolean }
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
    get.mockImplementation(async (path: string) => ({
      data: path === configPath ? { alert: legacyConfig.alert, report } : settings[path]
    }))
    put.mockImplementation(async (_path: string, data: unknown) => ({ data }))
  })

  it.each([
    { initial: undefined, toggle: false, expected: false },
    { initial: undefined, toggle: true, expected: true },
    { initial: true, toggle: true, expected: false }
  ])('loads $initial and saves $expected (toggle=$toggle) with existing report settings', async ({ initial, toggle, expected }) => {
    if (initial !== undefined) report.monthly_usage_enabled = initial
    const wrapper = await open()
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
    report.monthly_usage_enabled = true
    const wrapper = await open()
    expect(wrapper.find(control).exists()).toBe(false)
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(put).toHaveBeenCalledWith(configPath, { alert: legacyConfig.alert, report })
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
        alert: legacyConfig.alert, report: { ...report, enabled: false }
      })
    }
  })
})
