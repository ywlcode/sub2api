import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { startVisiblePolling } from '../visiblePolling'

describe('visible polling', () => {
  let stop: (() => void) | undefined
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    stop?.()
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('skips hidden ticks and resumes overdue work once on return', () => {
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    const refresh = vi.fn()
    stop = startVisiblePolling(refresh, 60_000)
    vi.advanceTimersByTime(180_000)
    expect(refresh).not.toHaveBeenCalled()
    visibility.mockReturnValue('visible')
    document.dispatchEvent(new Event('visibilitychange'))
    document.dispatchEvent(new Event('visibilitychange'))
    expect(refresh).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(60_000)
    expect(refresh).toHaveBeenCalledTimes(2)
    stop()
    vi.advanceTimersByTime(120_000)
    document.dispatchEvent(new Event('visibilitychange'))
    expect(refresh).toHaveBeenCalledTimes(2)
  })

  it('does not refresh on a brief tab switch', () => {
    const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    const refresh = vi.fn()
    stop = startVisiblePolling(refresh, 60_000)
    vi.advanceTimersByTime(5_000)
    visibility.mockReturnValue('visible')
    document.dispatchEvent(new Event('visibilitychange'))
    expect(refresh).not.toHaveBeenCalled()
    vi.advanceTimersByTime(55_000)
    expect(refresh).toHaveBeenCalledTimes(1)
  })
})
