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

export function navigate(route: { page: 'servers' } | { page: 'server'; id: string | number; tab: Tab }) {
  window.location.hash =
    route.page === 'servers'
      ? '/'
      : `/servers/${route.id}${route.tab === 'users' ? '' : `/${route.tab}`}`
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
