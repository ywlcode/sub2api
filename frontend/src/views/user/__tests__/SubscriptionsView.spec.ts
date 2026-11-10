import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import SubscriptionsView from '../SubscriptionsView.vue'

const mocks = vi.hoisted(() => ({
  push: vi.fn(),
  getMySubscriptions: vi.fn(),
  app: {
    cachedPublicSettings: { payment_enabled: false },
    fetchPublicSettings: vi.fn(),
    showInfo: vi.fn(),
    showError: vi.fn(),
  },
}))

vi.mock('vue-router', () => ({ useRouter: () => ({ push: mocks.push }) }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string, params?: { time?: string }) => params?.time ? `${key}: ${params.time}` : key }),
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks.app }))
vi.mock('@/api/subscriptions', () => ({
  default: {
    getMySubscriptions: mocks.getMySubscriptions,
  },
}))

describe('subscription renewal navigation', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.getMySubscriptions.mockResolvedValue([
      { id: 1, group_id: 7, status: 'active', group: { name: 'Test', platform: 'openai' } },
    ])
    mocks.app.cachedPublicSettings = { payment_enabled: false }
    mocks.app.fetchPublicSettings.mockResolvedValue(null)
  })

  async function clickRenew() {
    const wrapper = shallowMount(SubscriptionsView, {
      global: { stubs: { AppLayout: { template: '<div><slot /></div>' } } },
    })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    wrapper.unmount()
  }

  it('stays on subscriptions and explains when payment is disabled', async () => {
    await clickRenew()
    expect(mocks.app.fetchPublicSettings).toHaveBeenCalledWith(true)
    expect(mocks.push).not.toHaveBeenCalled()
    expect(mocks.app.showInfo).toHaveBeenCalledWith('purchase.notEnabledDesc')
  })

  it('uses refreshed settings and opens the matching subscription group', async () => {
    mocks.app.fetchPublicSettings.mockImplementation(async () => {
      mocks.app.cachedPublicSettings = { payment_enabled: true }
    })
    await clickRenew()
    expect(mocks.push).toHaveBeenCalledWith({
      path: '/purchase', query: { tab: 'subscription', group: '7' },
    })
    expect(mocks.app.showInfo).not.toHaveBeenCalled()
  })
})

describe('Beijing subscription reset countdowns', () => {
  it('uses server reset times for every window, including a one-day subscription', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-02-28T23:00:00+08:00'))
    mocks.getMySubscriptions.mockResolvedValue([{
      id: 1, group_id: 7, status: 'active',
      starts_at: '2026-02-28T18:00:00+08:00',
      expires_at: '2026-03-01T18:00:00+08:00',
      daily_window_start: '2026-02-28T18:00:00+08:00',
      weekly_window_start: '2026-02-28T18:00:00+08:00',
      monthly_window_start: '2026-02-28T18:00:00+08:00',
      daily_resets_at: '2026-03-01T00:00:00+08:00',
      weekly_resets_at: '2026-03-02T00:00:00+08:00',
      monthly_resets_at: '2026-03-01T00:00:00+08:00',
      group: { name: 'Test', platform: 'deepseek', daily_limit_usd: 10, weekly_limit_usd: 50, monthly_limit_usd: 100 },
    }])
    const wrapper = shallowMount(SubscriptionsView, {
      global: { stubs: { AppLayout: { template: '<div><slot /></div>' } } },
    })
    try {
      await flushPromises()
      expect(wrapper.text().match(/userSubscriptions.resetIn: 1h 0m/g)).toHaveLength(2)
      expect(wrapper.text()).toContain('userSubscriptions.resetIn: 1d 1h')
      expect(wrapper.text()).not.toContain('userSubscriptions.quotaEndsIn')
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
    }
  })
})
