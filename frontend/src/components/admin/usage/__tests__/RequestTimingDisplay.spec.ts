import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import UserRequestLatency from '../UserRequestLatency.vue'
import UsageStatsCards from '../UsageStatsCards.vue'
import UserDashboardStats from '@/components/user/dashboard/UserDashboardStats.vue'
import zh from '@/i18n/locales/zh/dashboard'
import en from '@/i18n/locales/en/dashboard'
import type { UsageLog, UsageStatsResponse } from '@/types'
import type { UserDashboardStats as DashboardStats } from '@/api/usage'

const global = (locale: 'en' | 'zh' = 'zh') => ({
  plugins: [createI18n({ legacy: false, locale, messages: { zh, en } })],
  stubs: { Icon: true },
})

describe('request phase display', () => {
  it('shows upload, processing, first token and TPS in the requested order', () => {
    const row = {
      content_timing: { request_phases: { user_upload_ms: 5000, processing_ms: 90_000 } },
      queue_timing: { status: 'measured', server_first_token_ms: 32_050 },
      first_token_ms: 50, duration_ms: 90_000, output_tokens: 2025,
    } as UsageLog
    const wrapper = mount(UserRequestLatency, { props: { row, showCacheRate: false }, global: global() })
    expect(wrapper.findAll('span').map(item => item.text())).toEqual([
      '用户上传耗时', '5s', '处理返回耗时', '1m 30s', '首字', '32.05s', '平均 TPS', '22.5 t/s',
    ])
    wrapper.unmount()
  })

  it('does not infer upload or processing from an old total or queue wait', async () => {
    const wrapper = mount(UserRequestLatency, {
      props: { row: { duration_ms: 95_000, queue_wait_ms: 5000 } as UsageLog, showCacheRate: false }, global: global(),
    })
    expect(wrapper.get('[data-testid="user-upload-duration"]').text()).toBe('—')
    expect(wrapper.get('[data-testid="processing-duration"]').text()).toBe('—')
    await wrapper.setProps({ row: { content_timing: { request_phases: { user_upload_ms: 0, processing_ms: 0 } } } as UsageLog })
    expect(wrapper.get('[data-testid="user-upload-duration"]').text()).toBe('0ms')
    expect(wrapper.get('[data-testid="processing-duration"]').text()).toBe('0ms')
    wrapper.unmount()
  })

  it.each(['en', 'zh'] as const)('uses the same measured averages in usage and dashboard in %s', locale => {
    const stats = { average_duration_ms: 777_000, average_user_upload_ms: 2000, average_processing_ms: 6000, timing_sample_count: 2 }
    const usage = mount(UsageStatsCards, { props: { stats: stats as UsageStatsResponse, showRequestPhases: true }, global: global(locale) })
    const dashboard = mount(UserDashboardStats, { props: { stats: stats as DashboardStats, balance: 0, isSimple: false }, global: global(locale) })
    for (const wrapper of [usage, dashboard]) {
      expect(wrapper.get('[data-testid="average-user-upload"]').text()).toContain('2s')
      expect(wrapper.get('[data-testid="average-processing"]').text()).toContain('6s')
      expect(wrapper.text()).not.toContain('777')
      wrapper.unmount()
    }
  })

  it('does not turn unavailable averages into zero', () => {
    const wrapper = mount(UsageStatsCards, { props: { stats: null, showRequestPhases: true }, global: global() })
    expect(wrapper.get('[data-testid="average-user-upload"] dd').text()).toBe('—')
    expect(wrapper.get('[data-testid="average-processing"] dd').text()).toBe('—')
    wrapper.unmount()
  })
})
