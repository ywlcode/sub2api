import { describe, expect, it } from 'vitest'
import { volcengineBaseUrls } from '../volcengine'

describe('Volcengine protocol endpoints', () => {
  it('replaces DeepSeek defaults with native Ark endpoints', () => {
    expect(volcengineBaseUrls('https://api.deepseek.com', {
      chat_completions: 'https://api.deepseek.com',
      responses: 'https://api.deepseek.com',
      anthropic: 'https://api.deepseek.com/anthropic'
    })).toEqual({
      chat_completions: 'https://ark.cn-beijing.volces.com/api/v3',
      responses: 'https://ark.cn-beijing.volces.com/api/v3',
      anthropic: 'https://ark.cn-beijing.volces.com/api/compatible'
    })
  })

  it('preserves a legacy relay and leaves its unknown Messages endpoint empty', () => {
    expect(volcengineBaseUrls('https://relay.example/v1/responses')).toEqual({
      chat_completions: 'https://relay.example/v1',
      responses: 'https://relay.example/v1/responses',
      anthropic: ''
    })
  })

  it('preserves explicitly configured protocol endpoints', () => {
    const urls = {
      chat_completions: 'https://chat.example/v1',
      responses: 'https://responses.example/api',
      anthropic: 'https://messages.example/anthropic'
    }
    expect(volcengineBaseUrls('https://legacy.example', urls)).toEqual(urls)
  })
})
