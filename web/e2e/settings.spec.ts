import { AMS, FRA, HEL, dialog, expect, hexOf, openServer, test, toast } from './fixtures'

async function aliceLink(page: import('@playwright/test').Page) {
  await page.getByRole('button', { name: 'Actions for alice' }).click()
  await page.getByRole('menuitem', { name: 'Share link' }).click()
  const share = dialog(page, "Share alice's proxy")
  const link = (await share.locator('.font-mono').first().textContent()) ?? ''
  await share.getByRole('button', { name: 'Close' }).click()
  return link
}

test('changing the cover site checks it first and changes every link', async ({ page }) => {
  await openServer(page, FRA)
  expect(await aliceLink(page)).toContain(hexOf('www.example.com'))
  await page.getByRole('button', { name: 'Settings' }).click()
  await expect(page).toHaveURL(/#\/servers\/1\/settings$/)
  await expect(page.getByText('www.example.com', { exact: true }).first()).toBeVisible()

  const check = page.getByRole('button', { name: 'Check' })
  const save = page.getByRole('button', { name: 'Use this site' })
  await expect(check).toBeDisabled()
  await page.getByLabel('New cover site').fill('bad.example.net')
  await check.click()
  await expect(page.getByText('bad.example.net does not offer TLS 1.3')).toBeVisible()
  await expect(save).toBeDisabled()

  await page.getByLabel('New cover site').fill('learn.example.net')
  await expect(save).toBeDisabled()
  await check.click()
  await expect(page.getByText('TLS 1.3', { exact: true })).toBeVisible()
  await expect(page.getByText('192.0.2.10, 2001:db8::10')).toBeVisible()
  await save.click()
  await expect(toast(page, 'Cover site is now learn.example.net')).toBeVisible()

  await page.getByRole('button', { name: 'Users' }).click()
  expect(await aliceLink(page)).toContain(hexOf('learn.example.net'))
})

test('the cover site cannot be checked while the link is down', async ({ page }) => {
  await openServer(page, AMS, 'settings')
  await page.getByLabel('New cover site').fill('learn.example.net')
  await page.getByRole('button', { name: 'Check' }).click()
  await expect(page.getByText('the link to this server is down')).toBeVisible()
})

test("sets the stream limit within the router's budget", async ({ page }) => {
  await openServer(page, FRA, 'settings')
  const limit = page.getByLabel('Stream limit')
  await expect(limit).toHaveValue('2000')
  await expect(page.getByText('Up to 4000 on this server')).toBeVisible()
  await limit.fill('4500')
  await page.getByRole('button', { name: 'Save limit' }).click()
  await expect(page.getByText('only 4000 streams are left on this router')).toBeVisible()
  await limit.fill('3000')
  await page.getByRole('button', { name: 'Save limit' }).click()
  await expect(toast(page, 'Stream limit set to 3000')).toBeVisible()
  await expect(page.getByText('Up to 4000 on this server')).toBeVisible()
})

test('shows and refreshes the allowlist', async ({ page }) => {
  await openServer(page, FRA, 'settings')
  const card = page.locator('section', { has: page.getByRole('heading', { name: 'Telegram allowlist' }) })
  await expect(card).toContainText('1 prefix')
  await expect(card).toContainText('placeholder')
  await expect(card).toContainText('never')
  await card.getByRole('button', { name: 'Refresh now' }).click()
  await expect(toast(page, 'Allowlist refreshed: 12 prefixes')).toBeVisible()
  await expect(card).toContainText('12 prefixes')
  await expect(card).not.toContainText('never')
})

test('shows the server details', async ({ page }) => {
  await openServer(page, FRA, 'settings')
  const details = page.locator('dl')
  await expect(details.getByText('root@203.0.113.10:22')).toBeVisible()
  await expect(details.getByText('SHA256:p2OiwWbYoEAsmmYWEMy1L12ya7lUDzuHYODmtJ18R/w')).toBeVisible()
  await expect(details.getByText('8443', { exact: true })).toBeVisible()
  await expect(details.getByText('amd64', { exact: true })).toBeVisible()
})

test('a cover site nobody has checked yet is not called failing', async ({ page }) => {
  await openServer(page, HEL, 'settings')
  const cover = page.locator('main').getByText('static.example.net', { exact: true }).locator('..')
  await expect(cover).toContainText('not checked yet')
  await expect(cover).not.toContainText('failing')
  await openServer(page, HEL, 'status')
  await expect(page.getByRole('definition').filter({ hasText: 'static.example.net' })).toContainText('not checked yet')
})

test('refresh is off when no list URL is configured', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { allowlistUrl: '' } })
  await openServer(page, FRA, 'settings')
  const card = page.locator('section', { has: page.getByRole('heading', { name: 'Telegram allowlist' }) })
  await expect(card).toContainText('built-in list only')
  await expect(card.getByRole('button', { name: 'Refresh now' })).toBeDisabled()
})

test('saving a stream limit warns that the server reconnects', async ({ page }) => {
  await openServer(page, FRA, 'settings')
  await expect(page.getByText(/reconnects the server/)).toBeVisible()
})
