import { describe, it, expect } from 'vitest'
import { getRollingRangeDates, getTodayRangeDates } from '../dateRange'

describe('page date ranges', () => {
  it('shares rolling presets within 30 seconds and advances at the boundary', () => {
    const now = Date.parse('2026-11-05T16:24:01Z')
    const first = getRollingRangeDates(86_400_000, now)
    expect(first).toEqual(getRollingRangeDates(86_400_000, now + 28_999))
    const next = getRollingRangeDates(86_400_000, now + 29_000)
    expect(Date.parse(next.end) - Date.parse(first.end)).toBe(30_000)
    expect(Date.parse(first.end) - Date.parse(first.start)).toBe(86_400_000)
  })

  it('resolves today with local calendar dates including after midnight', () => {
    expect(getTodayRangeDates(new Date(2026, 10, 5, 23, 59, 59))).toEqual({ start: '2026-11-05', end: '2026-11-05' })
    expect(getTodayRangeDates(new Date(2026, 10, 6, 0, 0, 1))).toEqual({ start: '2026-11-06', end: '2026-11-06' })
  })
})
