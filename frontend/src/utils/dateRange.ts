// Relative presets share cache keys within a 30-second window. Explicit custom
// timestamps bypass this helper and retain their original precision.
export const alignedRangeNow = (now = Date.now()): Date =>
  new Date(Math.floor(now / 30_000) * 30_000)

export const getRollingRangeDates = (durationMs = 86_400_000, now = Date.now()) => {
  const end = alignedRangeNow(now)
  return { start: new Date(end.getTime() - durationMs).toISOString(), end: end.toISOString() }
}

export const getTodayRangeDates = (now = new Date()) => {
  const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`
  return { start: today, end: today }
}
