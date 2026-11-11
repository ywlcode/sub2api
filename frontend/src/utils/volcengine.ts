// Preserve custom relay addresses; provider defaults are never valid Ark fallbacks.
export function volcengineBaseUrls(legacyBase: string, configured: Record<string, unknown> = {}) {
  const usable = (value: unknown): string => {
    if (typeof value !== 'string' || !value.trim()) return ''
    try {
      return new URL(value.trim()).hostname === 'api.deepseek.com' ? '' : value.trim()
    } catch {
      return value.trim() // Existing form/backend URL validation reports invalid input.
    }
  }
  const responses = usable(configured.responses) || usable(legacyBase) || 'https://ark.cn-beijing.volces.com/api/v3'
  let common = responses
  let anthropic = usable(configured.anthropic)
  try {
    const url = new URL(responses)
    url.pathname = url.pathname.replace(/\/$/, '').replace(/\/(responses|chat\/completions)$/, '')
    common = url.toString().replace(/\/$/, '')
    if (!anthropic && url.protocol === 'https:' && url.hostname === 'ark.cn-beijing.volces.com') {
      url.pathname = '/api/compatible'
      anthropic = url.toString()
    }
  } catch { /* URL validation belongs to the form/backend. */ }
  return { chat_completions: usable(configured.chat_completions) || common, responses, anthropic }
}
