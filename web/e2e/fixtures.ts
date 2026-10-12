import { test as base, expect, type Page } from '@playwright/test'

/** Every test starts from the mock's sample data and fails on page errors. */
export const test = base.extend<{ errors: string[] }>({
  errors: [
    async ({ page, request }, use) => {
      await request.post('/api/__reset')
      const errors: string[] = []
      page.on('pageerror', (e) => errors.push(e.message))
      page.on('console', (m) => m.type() === 'error' && !isExpectedHttpError(m.text()) && errors.push(m.text()))
      await use(errors)
      expect(errors, 'the page logged errors').toEqual([])
    },
    { auto: true },
  ],
})

// The browser logs failed requests (e.g. a down server's 503, or a job stream
// it could not open) as console errors; the UI handles those, so they are not failures.
function isExpectedHttpError(text: string) {
  return (
    /Failed to load resource: the server responded with a status of (4|5)\d\d/.test(text) ||
    /EventSource's response has a status (4|5)\d\d/.test(text)
  )
}

export { expect }

/** Sample servers: 1 Frankfurt (link up), 2 Helsinki (degraded), 3 Amsterdam (down). */
export const FRA = 1
export const HEL = 2
export const AMS = 3

export async function openServer(
  page: Page,
  id: number,
  tab: 'users' | 'status' | 'settings' | 'logs' = 'users',
) {
  await page.goto(`/#/servers/${id}${tab === 'users' ? '' : `/${tab}`}`)
}

export const dialog = (page: Page, name: string | RegExp) => page.getByRole('dialog', { name })

export const toast = (page: Page, text: string | RegExp) =>
  page.getByRole('status').filter({ hasText: text })

export const hexOf = (s: string) => Buffer.from(s).toString('hex')

/** The host key the mock's rebuilt and new servers present. */
export const NEW_KEY = 'SHA256:u3Rq0Q1uV3Jc0mYw7b6o8xk3j2m9Pq5c1f0aZyXwVb8'

/** The header the page sends on every API call; a test calling the API directly needs it too. */
export const PAGE = { 'X-MTProto-UI': '1' }

export const DOMESTIC_HINT =
  "This router reaches the server over its domestic route, so the server's address must be in the domestic address list."

/**
 * Whether any component's hook state on the page still holds `text` (a secret
 * kept too long). Walks Preact's tree from the root: the root element keeps its
 * vnode in `__k`, vnodes keep children in `__k` and their component in `__c`,
 * and a component keeps its hooks in `__H.__`, each with its value in `__`
 * (and a pending value in `__N`). These names are fixed by Preact's mangle map.
 */
export function stateHolds(page: Page, text: string) {
  return page.evaluate((needle) => {
    const holds = (v: unknown, depth: number): boolean => {
      if (typeof v === 'string') return v.includes(needle)
      if (!v || typeof v !== 'object' || depth > 4 || v instanceof Node) return false
      return Object.values(v).some((x) => holds(x, depth + 1))
    }
    const root = document.getElementById('root') as any
    // the whole tree, so a mounted component that renders nothing (a closed dialog) is seen too
    const stack: any[] = root?.__k ? [root.__k] : []
    if (!stack.length) throw new Error('no Preact tree on #root: did the internals change?')
    const seen = new Set<unknown>()
    let hooks = 0
    while (stack.length) {
      const v = stack.pop()
      if (!v || typeof v !== 'object' || seen.has(v)) continue
      seen.add(v)
      if (Array.isArray(v.__k)) stack.push(...v.__k)
      const list = v.__c?.__H?.__
      if (!Array.isArray(list)) continue
      hooks += list.length
      for (const h of list) if (holds(h.__, 0) || holds(h.__N, 0)) return true
    }
    // a walk that finds no hooks at all is looking in the wrong place
    if (!hooks) throw new Error('no hook state found: did the internals change?')
    return false
  }, text)
}
