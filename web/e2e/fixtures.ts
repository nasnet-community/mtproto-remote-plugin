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

// The browser logs failed fetches (e.g. an unreachable server's 502) as console
// errors; the UI handles those, so they are not failures.
function isExpectedHttpError(text: string) {
  return /Failed to load resource: the server responded with a status of (4|5)\d\d/.test(text)
}

export { expect }

export async function openServer(page: Page, id: string, tab: 'users' | 'settings' = 'users') {
  await page.goto(`/#/servers/${id}${tab === 'settings' ? '/settings' : ''}`)
}

export const dialog = (page: Page, name: string | RegExp) => page.getByRole('dialog', { name })

export const toast = (page: Page, text: string | RegExp) =>
  page.getByRole('status').filter({ hasText: text })
