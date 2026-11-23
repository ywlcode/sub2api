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
const revision = 'initial-saved-revision'
const storageKey = 'ops:manual-monthly:42'
function open(props: { reportRevision?: string } = {}) {
  return mount(MonthlyUsageSender, {
    props: { saved: report, draft: structuredClone(report), recipientInput: '', saving: false, reportRevision: revision, ...props },
    global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: { admin: zh, common: { cancel: '取消' } } } })] }
  })
}
const base = '/admin/ops/email-notification/monthly-usage'
const job: MonthlyUsageJob = { id: 'job-id', month: '2026-12', status: 'running', total: 3, accepted: 0, failed: 0, uncertain: 0 }
const response = (patch: Partial<MonthlyUsageJob> = {}) => ({ data: { ...job, ...patch } })
const postJob = (patch: Partial<MonthlyUsageJob> = {}) =>
  (_path: string, body: { request_id: string; month: string }) => response({ id: body.request_id, month: body.month, ...patch })
const getJob = (patch: Partial<MonthlyUsageJob> = {}) =>
  (path: string) => response({ id: path.split('/').pop(), ...patch })

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
    post.mockImplementation(postJob({ month: '2023-02', status: 'sent', accepted: 3 }))
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
    get.mockImplementationOnce(getJob({ accepted: 1 })).mockImplementationOnce(getJob({ status: 'sent', accepted: 3 }))
    const w = open()
    const button = w.get('button')
    await Promise.all([button.trigger('click'), button.trigger('click')])
    expect(button.element.disabled).toBe(true)
    expect(post).toHaveBeenCalledTimes(1)
    expect(post).toHaveBeenCalledWith(base + '/send', { month: job.month, expected_report_revision: revision, request_id: expect.stringMatching(/^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/) }, { signal: expect.any(AbortSignal) })
    resolve(response({ id: post.mock.calls[0][1].request_id }))
    await flushPromises()
    expect(w.text()).toContain('请求已受理')
    expect(w.text()).not.toContain('发送完成')
    expect(button.element.disabled).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(get).toHaveBeenCalledWith(base + '/jobs/' + post.mock.calls[0][1].request_id, { signal: expect.any(AbortSignal) })
    expect(w.text()).toContain('邮件服务器已接受 1')
    await vi.advanceTimersByTimeAsync(2000)
    expect(w.text()).toContain('发送完成')
    await vi.advanceTimersByTimeAsync(10000)
    expect(get).toHaveBeenCalledTimes(2)
  })

  it('retains one UUID through repeated POST timeouts and reopening, and requires a confirmed resend for a new ID', async () => {
    post.mockRejectedValueOnce({ status: 0 }).mockRejectedValueOnce({ status: 408 })
      .mockImplementationOnce(postJob({ status: 'uncertain', accepted: 1, failed: 1, uncertain: 1 }))
      .mockImplementationOnce(postJob({ status: 'sent', accepted: 3 }))
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
    await w.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(post).toHaveBeenCalledTimes(4)
    expect(post.mock.calls[3][1].request_id).not.toBe(id)
  })

  it('remembers both reported months on reopen and requires a separate activation for a resend', async () => {
    post.mockImplementation(postJob({ status: 'sent' }))
    let w = open()
    await clickSend(w)
    await w.get('input').setValue('2026-11')
    await clickSend(w)
    w.unmount()
    w = open()
    const send = w.get('button')
    for (const month of ['2026-12', '2026-11']) {
      await w.get('input').setValue(month)
      expect(send.text()).toBe('再次手动发送')
      await send.trigger('click')
      expect(w.text()).toContain(`再次发送 ${month} 月报`)
      const confirm = w.get('button.btn-primary')
      expect(confirm.element).not.toBe(send.element)
      const repeat = new KeyboardEvent('keydown', { key: 'Enter', repeat: true, cancelable: true, bubbles: true })
      expect(confirm.element.dispatchEvent(repeat)).toBe(false)
      await send.trigger('click')
      confirm.element.dispatchEvent(new MouseEvent('click', { bubbles: true, detail: 2 }))
      expect(post).toHaveBeenCalledTimes(2)
      const cancel = w.findAll('button').find(button => button.text() === '取消')!
      await cancel.trigger('click')
      expect(w.find('button.btn-primary').exists()).toBe(false)
    }
    await send.trigger('click')
    await w.setProps({ reportRevision: 'new-revision' })
    expect(w.find('button.btn-primary').exists()).toBe(false)
    await send.trigger('click')
    await w.get('button.btn-primary').trigger('click')
    await flushPromises()
    const payloads = post.mock.calls.map(([, body]) => body)
    expect(payloads[2]).toMatchObject({ month: '2026-11', expected_report_revision: 'new-revision' })
    expect(new Set(payloads.map(p => p.request_id)).size).toBe(3)
  })

  it('preserves a timed-out request through throttling and retries its original revision after reopen', async () => {
    post.mockRejectedValueOnce({ status: 408 }).mockRejectedValueOnce({ status: 429 })
      .mockImplementationOnce(postJob({ status: 'sent' }))
    let w = open()
    await clickSend(w)
    await w.setProps({ reportRevision: 'later-revision' })
    await clickSend(w)
    expect(w.text()).toContain('重试受到限流')
    expect(w.get('input').element.disabled).toBe(true)
    w.unmount()
    w = open({ reportRevision: 'another-revision' })
    expect(w.get('button').text()).toBe('重试同一请求')
    await clickSend(w)
    const original = post.mock.calls[0][1]
    expect(original.expected_report_revision).toBe(revision)
    expect(post.mock.calls.map(([, body]) => body)).toEqual([original, original, original])
    expect(w.text()).toContain('发送完成')
  })

  it.each([
    [409, 'MONTHLY_USAGE_REPORT_REVISION_CONFLICT'],
    [409, 'MONTHLY_USAGE_REPORT_BUSY'],
    [428, 'MONTHLY_USAGE_REPORT_REVISION_REQUIRED']
  ])('looks up the original request after an ambiguous POST and a %i %s retry', async (status, reason) => {
    post.mockRejectedValueOnce({ status: 0 }).mockRejectedValueOnce({ status, reason })
    let w = open()
    await clickSend(w)
    await w.setProps({ reportRevision: 'new-revision' })
    await clickSend(w)
    expect(w.get('button').text()).toBe('按原请求 ID 查询')
    expect(w.get('input').element.disabled).toBe(true)
    const original = post.mock.calls[0][1]
    expect(post.mock.calls[1][1]).toEqual(original)
    w.unmount()
    w = open({ reportRevision: 'new-revision' })
    get.mockImplementationOnce(getJob({ status: 'sent' }))
    await clickSend(w)
    expect(post).toHaveBeenCalledTimes(2)
    expect(get).toHaveBeenCalledWith(base + '/jobs/' + original.request_id, { signal: expect.any(AbortSignal) })
    expect(w.text()).toContain('发送完成')
    await clickSend(w)
    expect(w.find('button.btn-primary').exists()).toBe(true)
    expect(post).toHaveBeenCalledTimes(2)
  })

  it('keeps looking up an uncertain ID through unrelated 404s and gates a new ID after job-not-found', async () => {
    post.mockRejectedValueOnce({ status: 408 })
      .mockRejectedValueOnce({ status: 409, reason: 'MONTHLY_USAGE_REPORT_REVISION_CONFLICT' })
      .mockImplementationOnce(postJob({ status: 'sent' }))
    get.mockRejectedValueOnce({ status: 404, message: 'Route not found' })
      .mockRejectedValueOnce({ status: 404, reason: 'MONTHLY_USAGE_REPORT_JOB_NOT_FOUND' })
    const w = open()
    await clickSend(w)
    const original = post.mock.calls[0][1]
    await w.setProps({ reportRevision: 'reloaded' })
    await clickSend(w)
    await clickSend(w)
    expect(w.get('button').text()).toBe('按原请求 ID 查询')
    expect(w.get('input').element.disabled).toBe(true)
    await clickSend(w)
    expect(w.get('button').text()).toBe('再次手动发送')
    await clickSend(w)
    expect(post).toHaveBeenCalledTimes(2)
    await w.get('button.btn-primary').trigger('click')
    expect(post.mock.calls[2][1]).toMatchObject({ month: original.month, expected_report_revision: 'reloaded' })
    expect(post.mock.calls[2][1].request_id).not.toBe(original.request_id)
  })

  it('ignores unrelated job IDs from both submission and status responses', async () => {
    post.mockImplementationOnce(postJob({ id: 'wrong-id', status: 'sent' })).mockImplementationOnce(postJob())
    get.mockImplementationOnce(getJob({ id: 'wrong-id', status: 'sent' })).mockImplementationOnce(getJob({ status: 'sent' }))
    const w = open()
    await clickSend(w)
    expect(w.text()).toContain('未能确认请求是否已受理')
    expect(w.get('input').element.disabled).toBe(true)
    await clickSend(w)
    expect(post.mock.calls[1][1]).toEqual(post.mock.calls[0][1])
    await vi.advanceTimersByTimeAsync(2000)
    expect(w.text()).toContain('未能查询最新状态')
    expect(w.text()).not.toContain('发送完成')
    await w.get('[role="status"] button').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('发送完成')
  })

  it.each([undefined, '', '   '])('blocks a fresh send without a loaded report revision (%s)', async reportRevision => {
    const w = open({ reportRevision })
    expect(w.text()).toContain('升级后端')
    expect(w.get('button').element.disabled).toBe(true)
    await clickSend(w)
    expect(post).not.toHaveBeenCalled()
  })

  it.each([revision, undefined])('only looks up a legacy unresolved request by its ID (loaded revision %s)', async reportRevision => {
    sessionStorage.setItem(storageKey, JSON.stringify({ month: job.month, requestId: job.id, job: null }))
    get.mockRejectedValueOnce({ status: 429 }).mockImplementationOnce(getJob({ status: 'sent' }))
    const w = open({ reportRevision })
    expect(w.get('input').element.disabled).toBe(true)
    expect(w.get('button').text()).toBe('按原请求 ID 查询')
    await clickSend(w)
    expect(w.text()).toContain('请稍后再查')
    await clickSend(w)
    expect(get.mock.calls.map(([path]) => path)).toEqual([base + '/jobs/job-id', base + '/jobs/job-id'])
    expect(post).not.toHaveBeenCalled()
    expect(w.text()).toContain('发送完成')
    expect(w.get('button').element.disabled).toBe(!reportRevision)
  })

  it.each([
    { message: 'Monthly usage report job not found' },
    { reason: 'MONTHLY_USAGE_REPORT_JOB_NOT_FOUND' }
  ])('requires a new confirmation after an explicit missing-job response for a legacy request (%o)', async error => {
    sessionStorage.setItem(storageKey, JSON.stringify({ month: job.month, requestId: job.id, job: null }))
    get.mockRejectedValueOnce({ status: 404, ...error })
    const w = open()
    await clickSend(w)
    expect(w.text()).toContain('未找到原请求的任务')
    await clickSend(w)
    expect(post).not.toHaveBeenCalled()
    post.mockImplementationOnce(postJob({ status: 'sent' }))
    await w.get('button.btn-primary').trigger('click')
    expect(post.mock.calls[0][1]).toMatchObject({ month: job.month, expected_report_revision: revision })
    expect(post.mock.calls[0][1].request_id).not.toBe(job.id)
  })

  it.each([
    [409, 'MONTHLY_USAGE_REPORT_BUSY', '已有其他发送任务'],
    [409, 'MONTHLY_USAGE_REPORT_ID_CONFLICT', '原请求 ID 已用于其他月份'],
    [409, 'MONTHLY_USAGE_REPORT_REVISION_CONFLICT', '报告配置已变更'],
    [428, 'MONTHLY_USAGE_REPORT_REVISION_REQUIRED', '服务器要求提供报告配置版本'],
    [429, '', '发送请求过于频繁']
  ])('shows the rejection for %i %s and allows a fresh request', async (status, reason, hint) => {
    post.mockRejectedValueOnce({ status, reason })
    const w = open()
    await clickSend(w)
    expect(w.text()).toContain(hint)
    expect(w.text()).not.toContain('发送完成')
    expect(w.get('input').element.disabled).toBe(false)
    await vi.advanceTimersByTimeAsync(10000)
    expect(get).not.toHaveBeenCalled()
    await w.setProps({ reportRevision: 'reloaded' })
    post.mockImplementationOnce(postJob({ status: 'sent' }))
    await clickSend(w)
    expect(post.mock.calls[1][1].expected_report_revision).toBe('reloaded')
    expect(post.mock.calls[1][1].request_id).not.toBe(post.mock.calls[0][1].request_id)
  })

  it('retries a failed status lookup with GET only and displays the final failure counts', async () => {
    post.mockImplementationOnce(postJob())
    get.mockRejectedValueOnce(new Error('timeout')).mockImplementationOnce(getJob({ status: 'failed', failed: 3 }))
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
    post.mockImplementationOnce(postJob())
    get.mockReturnValueOnce(new Promise(r => { resolve = r })).mockImplementationOnce(getJob())
    const first = open()
    await clickSend(first)
    await vi.advanceTimersByTimeAsync(2000)
    first.unmount()
    expect(get.mock.calls[0][1].signal.aborted).toBe(true)
    resolve(response({ id: post.mock.calls[0][1].request_id, status: 'failed' }))
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
