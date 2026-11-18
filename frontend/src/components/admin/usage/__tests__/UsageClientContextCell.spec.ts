import { describe, it, expect, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import UsageClientContextCell from '../UsageClientContextCell.vue'
import zh from '@/i18n/locales/zh/dashboard'
import { usageContextIDs, usageSourceKinds, usageRequestSourceText } from '@/utils/usageClientContext'
import type { UsageClientContext } from '@/types'

const mocks = vi.hoisted(() => ({ copy: vi.fn().mockResolvedValue(undefined), success: vi.fn(), error: vi.fn() }))
vi.mock('@vueuse/core', () => ({ useClipboard: () => ({ copy: mocks.copy }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success, showError: mocks.error }) }))

function render(column: 'session' | 'source', context?: UsageClientContext, session?: string) {
  return mount(UsageClientContextCell, {
    props: { column, row: { client_context: context, session_id: session, user_agent: 'codex_cli_rs/1' } },
    global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: { ...zh, keys: { copied: '已复制', copyToClipboard: '复制' }, common: { copyFailed: '复制失败' } } } })] },
  })
}

describe('usage session and request source columns', () => {
  it('shows shared session separately from child and parent, and copies the full ID', async () => {
    const id = 'session-012345678901234567890'
    const wrapper = render('session', { session_id: id, thread_id: 'child-thread', parent_thread_id: 'main-thread' })
    expect(wrapper.text()).toContain('会话')
    expect(wrapper.text()).toContain('线程')
    expect(wrapper.text()).toContain('父线程')
    expect(wrapper.text()).toContain('child-thread')
    expect(wrapper.text()).not.toContain(id)
    await wrapper.find('button').trigger('click')
    await flushPromises()
    expect(mocks.copy).toHaveBeenCalledWith(id)
  })

  it('shows legacy sessions while leaving their agent role unknown', () => {
    expect(render('session', undefined, 'old-session').text()).toContain('old-session')
    const source = render('source')
    expect(source.text()).toContain('Codex')
    expect(source.text()).toContain('未识别')
    expect(source.text()).not.toContain('主代理')
    expect(render('session').text()).toContain('未提供')
  })

  it('keeps subagent and compaction orthogonal and safely escapes labels', () => {
    const wrapper = render('source', { client: 'claude_code', agent_role: 'subagent', request_kind: 'compaction', agent_type: '<img src=x onerror=alert(1)>' })
    expect(wrapper.text()).toContain('Claude Code')
    expect(wrapper.text()).toContain('子代理')
    expect(wrapper.text()).toContain('压缩')
    expect(wrapper.find('img').exists()).toBe(false)
  })

  it('does not resurrect a conflicting ID from the legacy column', () => {
    const wrapper = render('session', { identity_status: 'conflict' }, 'wrong-legacy-id')
    expect(wrapper.text()).toContain('标识冲突')
    expect(wrapper.text()).not.toContain('wrong-legacy-id')
  })

  it('only labels an explicitly declared main agent as main', () => {
    expect(usageSourceKinds({ request_kind: 'turn' })).toEqual(['unknown'])
    expect(usageSourceKinds({ agent_role: 'main', request_kind: 'turn' })).toEqual(['main'])
    expect(usageSourceKinds({ agent_role: 'subagent', request_kind: 'compaction' })).toEqual(['subagent', 'compaction'])
  })

  it('retains both siblings and parent information within the shared session', () => {
    const base = { session_id: 'root', parent_thread_id: 'main', agent_role: 'subagent' as const }
    const first = usageContextIDs({ user_agent: null, client_context: { ...base, thread_id: 'child-a' } })
    const second = usageContextIDs({ user_agent: null, client_context: { ...base, thread_id: 'child-b' } })
    expect(first[0]).toEqual(second[0])
    expect(first[1]).not.toEqual(second[1])
    expect(first[2]).toEqual(second[2])
    expect(usageRequestSourceText({ user_agent: 'dsh/1' }, key => key)).toBe('dsh · usage.attribution.unknown')
  })
})
