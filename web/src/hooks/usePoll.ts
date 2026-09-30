import { useCallback, useEffect, useRef, useState } from 'react'

/** Loads `fn` now and every `intervalMs` while the tab is visible. */
export function usePoll<T>(fn: () => Promise<T>, intervalMs: number, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const fnRef = useRef(fn)
  fnRef.current = fn

  const reload = useCallback(async () => {
    try {
      setData(await fnRef.current())
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    setLoading(true)
    setData(null)
    void reload()
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') void reload()
    }, intervalMs)
    return () => clearInterval(timer)
  }, [intervalMs, reload, ...deps])

  return { data, error, loading, reload }
}
