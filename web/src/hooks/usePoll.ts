import { useCallback, useEffect, useRef, useState } from 'react'

/** Loads `fn` now and every `intervalMs` while the tab is visible. */
export function usePoll<T>(fn: () => Promise<T>, intervalMs: number, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const fnRef = useRef(fn)
  fnRef.current = fn
  // bumped when deps change, so a reply for the old deps (another server) is dropped
  const generation = useRef(0)

  const reload = useCallback(async () => {
    const mine = generation.current
    try {
      const value = await fnRef.current()
      if (mine !== generation.current) return
      setData(value)
      setError(null)
    } catch (e) {
      if (mine !== generation.current) return
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      if (mine === generation.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    generation.current += 1
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
