import { useEffect, useState } from 'react'

export type Route = { page: 'servers' } | { page: 'server'; id: string; tab: 'users' | 'settings' }

export function parseRoute(hash: string): Route {
  const [, page, id, tab] = hash.replace(/^#/, '').split('/')
  if (page === 'servers' && id) {
    return { page: 'server', id, tab: tab === 'settings' ? 'settings' : 'users' }
  }
  return { page: 'servers' }
}

export function navigate(route: Route) {
  window.location.hash =
    route.page === 'servers' ? '/' : `/servers/${route.id}${route.tab === 'settings' ? '/settings' : ''}`
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parseRoute(window.location.hash))
  useEffect(() => {
    const onChange = () => setRoute(parseRoute(window.location.hash))
    window.addEventListener('hashchange', onChange)
    return () => window.removeEventListener('hashchange', onChange)
  }, [])
  return route
}
