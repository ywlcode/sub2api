import type { UsageClientContext, UsageLog } from '@/types'

export type UsageContextRow = Pick<UsageLog, 'session_id' | 'client_context' | 'user_agent'>
type Translate = (key: string) => string

const clients: Record<string, string> = { codex: 'Codex', claude_code: 'Claude Code', opencode: 'OpenCode', dsh: 'dsh' }

export function usageClientName(row: UsageContextRow): string {
  const declared = row.client_context?.client
  if (declared && clients[declared]) return clients[declared]!
  const ua = row.user_agent?.toLowerCase() ?? ''
  if (ua.includes('codex')) return clients.codex!
  if (ua.includes('claude-cli') || ua.includes('claude-code')) return clients.claude_code!
  if (ua.includes('opencode')) return clients.opencode!
  if (ua.includes('deepseek-harness') || ua.startsWith('dsh/')) return clients.dsh!
  return ''
}

export function usageSessionID(row: UsageContextRow): string {
  // Only legacy rows fall back to session_id. A new rejected identity must
  // not be resurrected from the older, less strict extractor.
  return row.client_context ? (row.client_context.session_id || row.client_context.thread_id || '') : (row.session_id || '')
}

export function usageContextIDs(row: UsageContextRow): { key: string; value: string }[] {
  const context = row.client_context
  const result: { key: string; value: string }[] = []
  const session = usageSessionID(row)
  if (session) result.push({ key: context?.thread_id && !context.session_id ? 'thread' : 'session', value: session })
  if (context?.thread_id && context.thread_id !== session) result.push({ key: 'thread', value: context.thread_id })
  if (context?.agent_id) result.push({ key: 'agent', value: context.agent_id })
  if (context?.parent_thread_id) result.push({ key: 'parentThread', value: context.parent_thread_id })
  if (context?.parent_agent_id) result.push({ key: 'parentAgent', value: context.parent_agent_id })
  return result
}

export function usageSourceKinds(context?: UsageClientContext | null): string[] {
  const result: string[] = []
  if (context?.agent_role === 'main' || context?.agent_role === 'subagent') result.push(context.agent_role)
  if (['compaction', 'auxiliary', 'workflow', 'prewarm', 'memory'].includes(context?.request_kind ?? '')) {
    result.push(context!.request_kind!)
  }
  return result.length ? result : ['unknown']
}

export function usageSessionText(row: UsageContextRow, t: Translate): string {
  return usageContextIDs(row).map(({ key, value }) => `${t(`usage.attribution.${key}`)}: ${value}`).join(' · ')
}

export function usageRequestSourceText(row: UsageContextRow, t: Translate): string {
  return [usageClientName(row) || t('usage.attribution.unknownClient'),
    ...usageSourceKinds(row.client_context).map(kind => t(`usage.attribution.${kind}`)),
    row.client_context?.agent_type,
  ].filter(Boolean).join(' · ')
}
