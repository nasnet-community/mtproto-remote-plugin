import { useEffect, useState } from 'react'

export type Tab = 'users' | 'status' | 'settings' | 'logs'
const TABS: Tab[] = ['users', 'status', 'settings', 'logs']

export type Route = { page: 'servers' } | { page: 'server'; id: string; tab: Tab }

export function parseRoute(hash: string): Route {
  const [, page, id, tab] = hash.replace(/^#/, '').split('/')
  if (page === 'servers' && id) {
    return { page: 'server', id, tab: TABS.includes(tab as Tab) ? (tab as Tab) : 'users' }
  }
  return { page: 'servers' }
}

// --- leaving with unsaved changes ---

const guards = new Map<string, string>()
let ask: ((message: string, go: () => void) => void) | null = null
// the hash the page shows, to undo a back button the user then cancels
let shown = typeof window === 'undefined' ? '' : window.location.hash
let passing = false

/** While set, leaving the page asks first; the message says what would be lost. */
export function setLeaveGuard(key: string, message: string | null) {
  if (message) guards.set(key, message)
  else guards.delete(key)
}

const guard = () => [...guards.values()].join(' ')

/** The App shows the question; `go` leaves. */
export function onLeave(fn: typeof ask) {
  ask = fn
}

function leave(go: () => void) {
  const message = guard()
  if (message && ask) ask(message, () => (guards.clear(), go()))
  else go()
}

export function navigate(route: { page: 'servers' } | { page: 'server'; id: string | number; tab: Tab }) {
  leave(() => {
    window.location.hash =
      route.page === 'servers' ? '/' : `/servers/${route.id}${route.tab === 'users' ? '' : `/${route.tab}`}`
  })
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parseRoute(window.location.hash))
  useEffect(() => {
    const onChange = () => {
      const next = window.location.hash
      if (guards.size && ask && !passing && next !== shown) {
        // put the address back (a full URL: a bare "#…" would resolve against <base>), then ask
        history.replaceState(null, '', window.location.href.split('#')[0] + shown)
        return leave(() => {
          passing = true
          window.location.hash = next
        })
      }
      passing = false
      shown = next
      setRoute(parseRoute(next))
    }
    const onUnload = (e: BeforeUnloadEvent) => {
      if (guards.size) e.preventDefault()
    }
    window.addEventListener('hashchange', onChange)
    window.addEventListener('beforeunload', onUnload)
    return () => {
      window.removeEventListener('hashchange', onChange)
      window.removeEventListener('beforeunload', onUnload)
    }
  }, [])
  return route
}
