import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh/admin/ops'
import MonthlyUsageSender from '../MonthlyUsageSender.vue'
import type { EmailNotificationConfig, MonthlyUsageJob } from '@/api/admin/ops'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get, post } }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ user: { id: 42 } }) }))
enableAutoUnmount(afterEach)

const report: EmailNotificationConfig['report'] = {
  enabled: true, recipients: ['reports@example.com'], monthly_usage_enabled: false, monthly_usage_department_attribute_id: 0,
  daily_summary_enabled: false, daily_summary_schedule: '', weekly_summary_enabled: false, weekly_summary_schedule: '',
  account_health_enabled: false, account_health_schedule: '', account_health_error_rate_threshold: 10,
  error_digest_enabled: false, error_digest_schedule: '', error_digest_min_count: 1
}
function open() {
  return mount(MonthlyUsageSender, {
    props: { saved: report, draft: structuredClone(report), recipientInput: '', saving: false },
    global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: { admin: zh, common: { cancel: '取消' } } } })] }
  })
}
const base = '/admin/ops/email-notification/monthly-usage'
const job: MonthlyUsageJob = { id: 'job-id', month: '2026-12', status: 'running', total: 3, accepted: 0, failed: 0, uncertain: 0 }
const response = (patch: Partial<MonthlyUsageJob> = {}) => ({ data: { ...job, ...patch } })
async function clickSend(w: ReturnType<typeof open>) {
  await w.get('button').trigger('click')
  await flushPromises()
}
beforeEach(() => {
  vi.resetAllMocks()
  sessionStorage.clear()
  vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] })
  vi.setSystemTime(new Date('2026-12-31T16:00:00Z'))
})
afterEach(() => { vi.useRealTimers() })

describe('manual monthly report', () => {
  it.each([
    ['2026-12-31T15:59:59Z', '2026-11'],
    ['2026-12-31T16:00:00Z', '2026-12'],
    ['2024-02-29T16:00:00Z', '2024-02']
  ])('defaults to the last completed Beijing month at %s', (now, expected) => {
    vi.setSystemTime(new Date(now))
    const input = open().get('input')
    expect(input.element.value).toBe(expected)
    expect(input.attributes('max')).toBe(expected)
  })

  it('blocks current/future/empty months and sends a selected older month', async () => {
    const w = open()
    for (const invalid of ['2027-01', '2027-02', '']) {
      await w.get('input').setValue(invalid)
      expect(w.get('button').element.disabled).toBe(true)
      await clickSend(w)
    }
    expect(post).not.toHaveBeenCalled()
    post.mockResolvedValue(response({ month: '2023-02', status: 'sent', accepted: 3 }))
    await w.get('input').setValue('2023-02')
    await clickSend(w)
    expect(post.mock.calls[0][1].month).toBe('2023-02')
  })

  it('requires saved enabled reports and recipients, but does not require automatic monthly email', async () => {
    const w = open()
    expect(w.get('button').element.disabled).toBe(false)
    for (const patch of [{ enabled: false }, { recipients: [] }]) {
      await w.setProps({ saved: { ...report, ...patch }, draft: { ...report, ...patch } })
      expect(w.get('button').element.disabled).toBe(true)
      expect(w.text()).toContain('请先开启评估报告')
    }
    await w.setProps({ saved: report, draft: { ...report, enabled: false } })
    expect(w.text()).toContain('请先保存')
    await w.setProps({ draft: report, saving: true })
    await clickSend(w)
    expect(post).not.toHaveBeenCalled()
  })

  it('blocks double submissions, polls only the accepted job, and stops on terminal status', async () => {
    let resolve!: (value: ReturnType<typeof response>) => void
    post.mockReturnValue(new Promise(r => { resolve = r }))
    get.mockResolvedValueOnce(response({ accepted: 1 })).mockResolvedValueOnce(response({ status: 'sent', accepted: 3 }))
    const w = open()
    const button = w.get('button')
    await Promise.all([button.trigger('click'), button.trigger('click')])
    expect(button.element.disabled).toBe(true)
    expect(post).toHaveBeenCalledTimes(1)
    expect(post).toHaveBeenCalledWith(base + '/send', { month: job.month, request_id: expect.stringMatching(/^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/) }, { signal: expect.any(AbortSignal) })
    resolve(response())
    await flushPromises()
    expect(w.text()).toContain('请求已受理')
    expect(w.text()).not.toContain('发送完成')
    expect(button.element.disabled).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(get).toHaveBeenCalledWith(base + '/jobs/job-id', { signal: expect.any(AbortSignal) })
    expect(w.text()).toContain('邮件服务器已接受 1')
    await vi.advanceTimersByTimeAsync(2000)
    expect(w.text()).toContain('发送完成')
    await vi.advanceTimersByTimeAsync(10000)
    expect(get).toHaveBeenCalledTimes(2)
  })

  it('retains one UUID through repeated POST timeouts and reopening, and requires a confirmed resend for a new ID', async () => {
    post.mockRejectedValueOnce({ status: 0 }).mockRejectedValueOnce({ status: 408 })
      .mockResolvedValueOnce(response({ status: 'uncertain', accepted: 1, failed: 1, uncertain: 1 }))
      .mockResolvedValueOnce(response({ status: 'sent', accepted: 3 }))
    let w = open()
    await clickSend(w)
    expect(w.text()).toContain('未能确认请求是否已受理')
    expect(w.get('input').element.disabled).toBe(true)
    await clickSend(w)
    w.unmount()
    w = open()
    expect(w.get('button').text()).toBe('重试同一请求')
    await clickSend(w)
    const id = post.mock.calls[0][1].request_id
    expect(post.mock.calls.map(c => c[1].request_id)).toEqual([id, id, id])
    expect(w.text()).toContain('收件人 3 · 邮件服务器已接受 1 · 失败 1 · 未知 1')
    expect(w.text()).toContain('投递结果未知')
    await clickSend(w)
    expect(post).toHaveBeenCalledTimes(3)
    expect(w.get('button').text()).toBe('确认再次发送')
    await clickSend(w)
    expect(post).toHaveBeenCalledTimes(4)
    expect(post.mock.calls[3][1].request_id).not.toBe(id)
  })

  it('treats a conflict as busy elsewhere and never polls or reports success for it', async () => {
    post.mockRejectedValueOnce({ status: 409 })
    const w = open()
    await clickSend(w)
    expect(w.text()).toContain('已有其他发送任务')
    expect(w.text()).not.toContain('发送完成')
    expect(w.get('input').element.disabled).toBe(false)
    await vi.advanceTimersByTimeAsync(10000)
    expect(get).not.toHaveBeenCalled()
  })

  it('retries a failed status lookup with GET only and displays the final failure counts', async () => {
    post.mockResolvedValueOnce(response())
    get.mockRejectedValueOnce(new Error('timeout')).mockResolvedValueOnce(response({ status: 'failed', failed: 3 }))
    const w = open()
    await clickSend(w)
    await vi.advanceTimersByTimeAsync(2000)
    expect(w.text()).toContain('未能查询最新状态')
    expect(w.get('button').element.disabled).toBe(true)
    await w.get('[role="status"] button').trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledTimes(1)
    expect(get).toHaveBeenCalledTimes(2)
    expect(w.text()).toContain('发送失败')
    expect(w.text()).toContain('失败 3')
  })

  it('aborts an in-flight lookup, ignores its late result, resumes on open and cancels its next timer on close', async () => {
    let resolve!: (value: ReturnType<typeof response>) => void
    post.mockResolvedValueOnce(response())
    get.mockReturnValueOnce(new Promise(r => { resolve = r })).mockResolvedValueOnce(response())
    const first = open()
    await clickSend(first)
    await vi.advanceTimersByTimeAsync(2000)
    first.unmount()
    expect(get.mock.calls[0][1].signal.aborted).toBe(true)
    resolve(response({ status: 'failed' }))
    await flushPromises()
    const reopened = open()
    await flushPromises()
    expect(get).toHaveBeenCalledTimes(2)
    expect(reopened.text()).toContain('请求已受理')
    reopened.unmount()
    await vi.advanceTimersByTimeAsync(10000)
    expect(get).toHaveBeenCalledTimes(2)
    expect(post).toHaveBeenCalledTimes(1)
  })
})
