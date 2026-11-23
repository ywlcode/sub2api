import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
enableAutoUnmount(afterEach)

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

import OpsSettingsDialog from '../OpsSettingsDialog.vue'
const { opsAPI, showError } = vi.hoisted(() => ({
  opsAPI: {
    getAlertRuntimeSettings: vi.fn(), getEmailNotificationConfig: vi.fn(),
    getAdvancedSettings: vi.fn(), getMetricThresholds: vi.fn(),
    updateAlertRuntimeSettings: vi.fn(), updateEmailNotificationConfig: vi.fn(),
    updateAdvancedSettings: vi.fn(), updateMetricThresholds: vi.fn(),
  }, showError: vi.fn(),
}))
vi.mock('@/api/admin/ops', () => ({ opsAPI, default: opsAPI }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError, showSuccess: vi.fn() }) }))
async function openDialog() {
  const wrapper = shallowMount(OpsSettingsDialog, {
    props: { show: false },
    global: { stubs: { BaseDialog: { template: '<div><slot name="footer" /></div>' } } },
  })
  await wrapper.setProps({ show: true })
  return wrapper
}
type View = { saveAllSettings: () => Promise<void> }
describe('ops settings load guard', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    opsAPI.getAlertRuntimeSettings.mockResolvedValue({ evaluation_interval_seconds: 30 })
    opsAPI.getEmailNotificationConfig.mockResolvedValue({ report_revision: 'loaded', alert: { enabled: false }, report: { enabled: false } })
    opsAPI.updateEmailNotificationConfig.mockResolvedValue({ report_revision: 'saved', alert: { enabled: false }, report: { enabled: false } })
    opsAPI.getAdvancedSettings.mockResolvedValue({ data_retention: { error_log_retention_days: 7, minute_metrics_retention_days: 7, hourly_metrics_retention_days: 7 } })
    opsAPI.getMetricThresholds.mockResolvedValue({ sla_percent_min: 98 })
  })
  afterEach(() => { vi.restoreAllMocks() })
  it('blocks saving while any settings request is still loading', async () => {
    const pending = deferred<object>()
    opsAPI.getMetricThresholds.mockReturnValueOnce(pending.promise)
    const wrapper = await openDialog()
    expect(wrapper.get('button.btn-primary').attributes('disabled')).toBeDefined()
    await (wrapper.vm as unknown as View).saveAllSettings()
    expect(opsAPI.updateMetricThresholds).not.toHaveBeenCalled()
    pending.resolve({ sla_percent_min: 98 })
    await flushPromises()
    expect(wrapper.get('button.btn-primary').attributes('disabled')).toBeUndefined()
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(opsAPI.updateEmailNotificationConfig).toHaveBeenCalledWith(expect.objectContaining({ report_revision: 'loaded' }), undefined)
    expect(opsAPI.updateMetricThresholds).toHaveBeenCalledWith(expect.objectContaining({ sla_percent_min: 98 }))
    expect(wrapper.emitted('saved')).toHaveLength(1)
  })
  it('does not save default thresholds after a failed initial load', async () => {
    opsAPI.getMetricThresholds.mockRejectedValueOnce(new Error('load failed'))
    const wrapper = await openDialog()
    await flushPromises()
    expect(wrapper.get('button.btn-primary').attributes('disabled')).toBeDefined()
    await (wrapper.vm as unknown as View).saveAllSettings()
    expect(opsAPI.updateMetricThresholds).not.toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toBeUndefined()
  })
  it('blocks saving stale settings after a failed reopen and allows a later retry', async () => {
    const wrapper = await openDialog()
    await flushPromises()
    await wrapper.setProps({ show: false })
    opsAPI.getMetricThresholds.mockRejectedValueOnce(new Error('refresh failed'))
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(wrapper.get('button.btn-primary').attributes('disabled')).toBeDefined()
    await (wrapper.vm as unknown as View).saveAllSettings()
    expect(opsAPI.updateAlertRuntimeSettings).not.toHaveBeenCalled()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(wrapper.get('button.btn-primary').attributes('disabled')).toBeUndefined()
  })
  it('ignores an earlier load that completes after reopening with a newer revision', async () => {
    const first = deferred<object>()
    opsAPI.getEmailNotificationConfig.mockReturnValueOnce(first.promise)
    const wrapper = await openDialog()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await flushPromises()
    first.resolve({ report_revision: 'obsolete', alert: { enabled: false }, report: { enabled: false } })
    await flushPromises()
    await (wrapper.vm as unknown as View).saveAllSettings()
    expect(opsAPI.updateEmailNotificationConfig).toHaveBeenCalledWith(expect.objectContaining({ report_revision: 'loaded' }), undefined)
  })
  it('keeps save locked until parallel writes settle and settings are reloaded after a failure', async () => {
    const pending = deferred<object>()
    opsAPI.updateEmailNotificationConfig.mockReturnValueOnce(pending.promise)
    opsAPI.updateMetricThresholds.mockRejectedValueOnce(new Error('connection lost'))
    const wrapper = await openDialog()
    await flushPromises()
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    await (wrapper.vm as unknown as View).saveAllSettings()
    expect(opsAPI.updateEmailNotificationConfig).toHaveBeenCalledTimes(1)
    pending.resolve({ report_revision: 'saved', alert: { enabled: false }, report: { enabled: false } })
    await flushPromises()
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(wrapper.get('button.btn-primary').attributes('disabled')).toBeDefined()
    await (wrapper.vm as unknown as View).saveAllSettings()
    expect(opsAPI.updateEmailNotificationConfig).toHaveBeenCalledTimes(1)
  })
  it('waits for a previous save before fetching a reopened dialog', async () => {
    const pending = deferred<object>()
    opsAPI.updateEmailNotificationConfig.mockReturnValueOnce(pending.promise)
    const wrapper = await openDialog()
    await flushPromises()
    await wrapper.get('button.btn-primary').trigger('click')
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    expect(opsAPI.getEmailNotificationConfig).toHaveBeenCalledTimes(1)
    expect(wrapper.get('button.btn-primary').attributes('disabled')).toBeDefined()
    const current = { report_revision: 'current', alert: { enabled: false }, report: { enabled: false } }
    opsAPI.getEmailNotificationConfig.mockResolvedValueOnce(current)
    pending.resolve(current)
    await flushPromises()
    expect(opsAPI.getEmailNotificationConfig).toHaveBeenCalledTimes(2)
    expect(wrapper.emitted('close')).toBeUndefined()
    await (wrapper.vm as unknown as View).saveAllSettings()
    expect(opsAPI.updateEmailNotificationConfig).toHaveBeenLastCalledWith(current, undefined)
  })
})
