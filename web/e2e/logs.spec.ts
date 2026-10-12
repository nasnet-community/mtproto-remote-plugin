import { readFileSync } from 'node:fs'
import type { Page } from '@playwright/test'
import { AMS, FRA, HEL, expect, openServer, test } from './fixtures'

const log = (page: Page) => page.getByRole('log', { name: 'Log lines' })
const toEnd = (page: Page) => log(page).evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight)

test('shows the log newest at the bottom, with errors and warnings tinted', async ({ page }) => {
  await openServer(page, HEL, 'logs')
  await expect(log(page)).toContainText('cannot bind 0.0.0.0:443: address already in use')
  await expect(log(page).locator('div', { hasText: 'address already in use' }).first()).toHaveClass(/bg-bad-tint/)
  await expect(log(page).locator('div', { hasText: 'keeps crashing' }).first()).toHaveClass(/bg-warn-tint/)
  await expect(log(page).locator('div', { hasText: 'Telemt MTProxy' }).first()).not.toHaveClass(/tint/)
  // following: scrolled to the newest line
  await expect.poll(() => toEnd(page)).toBeLessThan(12)
  await expect(page.getByText("300 lines, kept in the agent's memory")).toBeVisible()

  await page.getByRole('button', { name: '100', exact: true }).click()
  await expect(page.getByRole('button', { name: '100', exact: true })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByText("100 lines, kept in the agent's memory")).toBeVisible()
  await page.getByRole('button', { name: '1,000 lines' }).click()
  await expect(page.getByText("1,000 lines, kept in the agent's memory")).toBeVisible()
})

test('search narrows the lines and marks what matched', async ({ page }) => {
  await openServer(page, FRA, 'logs')
  await expect(log(page)).toContainText('handshake ok user=alice')
  await page.getByLabel('Search the log').fill('ALICE')
  await expect(log(page).locator('mark').first()).toHaveText('alice')
  const rows = await log(page).locator(':scope > div').allTextContents()
  expect(rows.length).toBeGreaterThan(0)
  for (const r of rows) expect(r.toLowerCase()).toContain('alice')
  await page.getByLabel('Search the log').fill('no-such-text')
  await expect(log(page)).toContainText('No line matches.')
})

test('scrolling up pauses following; new lines wait until you come back', async ({ page }) => {
  await openServer(page, FRA, 'logs')
  const follow = page.getByRole('button', { name: 'Following new lines' })
  await expect(follow).toHaveAttribute('aria-pressed', 'true')
  await expect.poll(() => toEnd(page)).toBeLessThan(12)
  await log(page).evaluate((el) => el.scrollTo(0, 0))
  await expect(page.getByRole('button', { name: /^Paused/ })).toHaveAttribute('aria-pressed', 'false')
  // the mock writes a line every 5 s; it waits below
  const back = page.getByRole('button', { name: /^\d+ new lines?$/ })
  await expect(back).toBeVisible({ timeout: 15000 })
  expect(await log(page).evaluate((el) => el.scrollTop)).toBe(0)
  await back.click()
  await expect(follow).toBeVisible()
  await expect.poll(() => toEnd(page)).toBeLessThan(12)
})

test('downloads the log as a text file', async ({ page }) => {
  await openServer(page, HEL, 'logs')
  await expect(log(page)).toContainText('address already in use')
  const [file] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Download' }).click()])
  expect(file.suggestedFilename()).toMatch(/^helsinki-arm-log-\d{4}-\d\d-\d\d\.txt$/)
  const text = readFileSync(await file.path(), 'utf8')
  expect(text).toContain('cannot bind 0.0.0.0:443: address already in use')
  expect(text.trim().split('\n')).toHaveLength(300)
})

test('says why a down server has no logs', async ({ page }) => {
  await openServer(page, AMS, 'logs')
  await expect(page.getByText('the link to this server is down')).toBeVisible()
})
