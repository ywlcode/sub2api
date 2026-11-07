/** Poll display data only while visible, refreshing overdue data on return. */
export function startVisiblePolling(refresh: () => void, intervalMs: number): () => void {
  let stopped = false
  let lastRefreshAt = Date.now()
  let timer: ReturnType<typeof setTimeout>

  const tick = () => {
    if (stopped) return
    if (document.visibilityState !== 'hidden') {
      lastRefreshAt = Date.now()
      refresh()
    }
    if (!stopped) timer = setTimeout(tick, intervalMs)
  }
  const onVisibility = () => {
    if (document.visibilityState === 'hidden' || Date.now() - lastRefreshAt < intervalMs) return
    clearTimeout(timer)
    tick()
  }
  timer = setTimeout(tick, intervalMs)
  document.addEventListener('visibilitychange', onVisibility)
  return () => {
    stopped = true
    clearTimeout(timer)
    document.removeEventListener('visibilitychange', onVisibility)
  }
}
