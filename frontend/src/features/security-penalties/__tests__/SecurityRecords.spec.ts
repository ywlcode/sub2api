import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import securityRecords from '@/i18n/locales/en/securityRecords'
import SecurityRecords from '../SecurityRecords.vue'
import * as api from '../api'

vi.mock('../api', () => ({ getSummary: vi.fn(), listEvents: vi.fn(), getEvent: vi.fn(), listMyModerationRecords: vi.fn() }))
const labels = securityRecords.moderation
const moderationItem: api.ModerationRecord = {
  id: 5, action: 'block', created_at: '2026-10-21T10:30:00Z', model: 'example-model', category: 'example-category',
  violation_count: 7, ban_applied: true, ban_scope: 'openai', ban_expires_at: '2026-10-22T10:30:00Z',
  input_excerpt: '<strong>retained excerpt</strong>'
}
const moderationPage: api.ModerationRecordsPage = {
  page: 1, page_size: 10, total: 12, current_count: 1, threshold: 4, auto_ban_enabled: false,
  recorded_ban_count: 3, account_banned: false, openai_banned: true, items: [moderationItem]
}

function render(props: { admin?: boolean; userId?: number } = {}) {
  const i18n = createI18n({
    legacy: false, locale: 'en', messages: { en: {
      securityRecords, common: { refresh: 'Refresh', loading: 'Loading', enabled: 'Enabled', disabled: 'Disabled' },
      pagination: { pageOf: 'Page {page} of {total}' }
    } }
  })
  return mount(SecurityRecords, { props, global: { plugins: [i18n] } })
}

describe('personal security records', () => {
  beforeEach(() => {
    vi.resetAllMocks()
    vi.mocked(api.listMyModerationRecords).mockResolvedValue(moderationPage)
    vi.mocked(api.getSummary).mockResolvedValue({ enabled: true, observe_only: false, policy: 'session_block', threshold: 3, restricted: true, states: [{ scope: 'session', subject_key: 'hash-of-session', policy: 'session_block', count: 3, blocked_until: '2026-10-22T00:00:00Z', route_until: null, action: 'block', reason: 'jailbreak', updated_at: '2026-10-21T00:00:00Z' }] })
    vi.mocked(api.listEvents).mockResolvedValue({ page: 1, page_size: 10, total: 1, items: [{ id: 9, user_id: 7, api_key_id: 8, scope: 'session', policy: 'session_block', reason: 'jailbreak', count: 3, model: 'model', source: 'prompt_audit', created_at: '2026-10-21T00:00:00Z' }] })
  })
  it('shows restriction and cause, loading the original only on demand as text', async () => {
    vi.mocked(api.getEvent).mockResolvedValue({ id: 9, user_id: 7, api_key_id: 8, scope: 'session', policy: 'session_block', reason: 'jailbreak', count: 3, model: 'model', source: 'prompt_audit', created_at: '', full_prompt: '<script>danger()</script> original conversation' })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.get('[data-testid="restriction-status"]').text()).toBe(securityRecords.restricted)
    expect(wrapper.text()).toContain('jailbreak')
    expect(api.getEvent).not.toHaveBeenCalled()
    const expand = wrapper.findAll('button').find(button => button.text() === securityRecords.showPrompt)!
    await expand.trigger('click'); await flushPromises()
    expect(api.getEvent).toHaveBeenCalledWith(9, false)
    expect(wrapper.get('[data-testid="security-original-prompt"]').text()).toContain('<script>danger()</script>')
    expect(wrapper.find('script').exists()).toBe(false)
    expect(api.listEvents).toHaveBeenCalledWith(1, false, undefined)
  })
  it('reports unavailable status instead of claiming unrestricted when status loading fails', async () => {
    vi.mocked(api.getSummary).mockRejectedValue(new Error('offline'))
    const wrapper = render(); await flushPromises()
    expect(wrapper.find('[data-testid="restriction-status"]').exists()).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toBe(securityRecords.loadFailed)
  })
  it('shows current moderation status independently of historical counts and safely expands the excerpt', async () => {
    vi.mocked(api.getSummary).mockResolvedValue({ enabled: true, observe_only: false, policy: 'session_block', threshold: 3, restricted: false, states: [] })
    const wrapper = render()
    await flushPromises()
    const section = wrapper.get('[data-testid="moderation-records"]')
    expect(section.text()).toContain(labels.openaiBanned)
    expect(section.text()).not.toContain(labels.allPlatformsBlocked)
    expect(wrapper.text()).not.toMatch(/account (disabled|suspended)|no active model request restriction/i)
    expect(section.text()).toContain('1 / 4')
    expect(section.text()).toContain('Disabled')
    expect(section.get('dl').text()).toContain(labels.totalBans + '3')
    expect(section.text()).not.toContain('retained excerpt')
    const item = section.get('article')
    expect(item.text()).toContain(labels.actions.block)
    expect(item.text()).toContain('example-model')
    expect(item.get('time').attributes('datetime')).toBe(moderationItem.created_at)
    await item.get('button').trigger('click')
    expect(item.text()).toContain('example-category')
    expect(item.text()).toContain(labels.historicalCount + '7')
    expect(item.text()).toContain(labels.banOutcome + labels.banApplied)
    expect(item.text()).toContain(labels.openaiOnly)
    expect(item.find('time[datetime="2026-10-22T10:30:00Z"]').exists()).toBe(true)
    expect(item.get('pre').text()).toBe(moderationItem.input_excerpt)
    expect(item.find('strong').exists()).toBe(false)
    await item.get('button').trigger('click')
    expect(item.find('pre').exists()).toBe(false)
  })

  it('shows the all-platform restriction when both scopes are reported', async () => {
    vi.mocked(api.listMyModerationRecords).mockResolvedValue({ ...moderationPage, account_banned: true, openai_banned: true })
    const wrapper = render()
    await flushPromises()
    const section = wrapper.get('[data-testid="moderation-records"]')
    expect(section.text()).toContain('model requests on all platforms')
    expect(section.text()).toContain('sign in and use your dashboard')
    expect(section.text()).not.toContain(labels.openaiBanned)
  })

  it.each([
    [null, labels.unknownOutcome],
    [false, labels.banNotApplied]
  ] as const)('distinguishes historical ban_applied=%s and missing evidence', async (ban_applied, expected) => {
    vi.mocked(api.listMyModerationRecords).mockResolvedValue({
      ...moderationPage, account_banned: true, openai_banned: false, auto_ban_enabled: true,
      items: [{ ...moderationItem, ban_applied, ban_scope: null, input_excerpt: '', ban_expires_at: null }]
    })
    const wrapper = render()
    await flushPromises()
    const section = wrapper.get('[data-testid="moderation-records"]')
    expect(section.text()).toContain(labels.allPlatformsBlocked)
    expect(section.text()).not.toContain(labels.noBan)
    expect(section.text()).toContain('Enabled')
    const item = section.get('article')
    await item.get('button').trigger('click')
    expect(item.text()).toContain(labels.banOutcome + expected)
    expect(item.text()).toContain(labels.noExcerpt)
    expect(item.find('pre').exists()).toBe(false)
    expect(item.text()).not.toContain(labels.scheduledRelease)
  })

  it('paginates moderation separately and retries the failed page without showing stale status', async () => {
    vi.mocked(api.listMyModerationRecords)
      .mockResolvedValueOnce(moderationPage)
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValueOnce({
        ...moderationPage, page: 2, items: [{ ...moderationItem, id: 14, ban_scope: 'all' }]
      })
    const wrapper = render()
    await flushPromises()
    const section = wrapper.get('[data-testid="moderation-records"]')
    const penalty = wrapper.get('[data-testid="security-records"]')
    await section.get('article button').trigger('click')
    await section.findAll('nav button')[1].trigger('click')
    await flushPromises()
    expect(section.get('[role=alert]').text()).toBe(labels.loadFailed)
    expect(section.text()).not.toContain(labels.openaiBanned)
    expect(section.text()).not.toContain(labels.empty)
    expect(section.find('article').exists()).toBe(false)
    expect(penalty.text()).toContain('jailbreak')
    await section.get('button').trigger('click')
    await flushPromises()
    expect(vi.mocked(api.listMyModerationRecords).mock.calls).toEqual([[1], [2], [2]])
    expect(api.listEvents).toHaveBeenCalledTimes(1)
    expect(section.text()).toContain('Page 2 of 2')
    expect(section.get('article').text()).toContain('#14')
    expect(section.find('pre').exists()).toBe(false)
    await section.get('article button').trigger('click')
    expect(section.text()).toContain(labels.allPlatforms)
    await penalty.get('button').trigger('click')
    await flushPromises()
    expect(section.text()).toContain('Page 2 of 2')
    expect(api.listMyModerationRecords).toHaveBeenCalledTimes(3)
  })

  it('keeps the admin panel scoped to admin penalty records', async () => {
    const wrapper = render({ admin: true, userId: 23 })
    await flushPromises()
    expect(wrapper.find('[data-testid="moderation-records"]').exists()).toBe(false)
    expect(api.getSummary).not.toHaveBeenCalled()
    expect(api.listMyModerationRecords).not.toHaveBeenCalled()
    expect(api.listEvents).toHaveBeenCalledWith(1, true, 23)
  })
})
