import type { Page } from '@playwright/test'
import { AMS, FRA, dialog, expect, openServer, test, toast } from './fixtures'

const row = (page: Page, name: string) =>
  page.getByRole('row').filter({ has: page.getByText(name, { exact: true }) })

async function userAction(page: Page, name: string, action: string) {
  await page.getByRole('button', { name: `Actions for ${name}` }).click()
  await page.getByRole('menuitem', { name: action }).click()
}

async function shareLink(page: Page, name: string) {
  await userAction(page, name, 'Share link')
  const share = dialog(page, `Share ${name}'s proxy`)
  const link = (await share.locator('.font-mono').first().textContent()) ?? ''
  await share.getByRole('button', { name: 'Close' }).click()
  return link
}

test.beforeEach(async ({ page }) => {
  await openServer(page, FRA)
  await expect(row(page, 'alice')).toBeVisible()
})

test('shows users, their usage and state', async ({ page }) => {
  await expect(page.locator('div').filter({ hasText: /^Users22$/ })).toHaveCount(1)
  await expect(row(page, 'bob')).toContainText('910 MB')
  await expect(row(page, 'alice')).toContainText('quota 50.0 GB')
  await expect(row(page, 'alice')).toContainText('active')
  await expect(row(page, 'old-phone')).toContainText('disabled')
  await expect(row(page, 'expired-trial')).toContainText('expired')
  await expect(row(page, 'used-up')).toContainText('over quota')
  await expect(row(page, 'bob')).toContainText('never')
  // Live sessions and client addresses are not reported, so not shown.
  await expect(page.getByRole('columnheader', { name: 'Connections' })).toHaveCount(0)
  await expect(page.getByRole('columnheader', { name: 'Devices' })).toHaveCount(0)
})

test('searches users', async ({ page }) => {
  await page.getByLabel('Search users').fill('trial')
  await expect(row(page, 'trial-7d')).toBeVisible()
  await expect(row(page, 'expired-trial')).toBeVisible()
  await expect(row(page, 'alice')).toHaveCount(0)
  await page.getByLabel('Search users').fill('nobody-matches')
  await expect(page.getByText('No user matches.')).toBeVisible()
})

test('adds a user with limits and shares the link', async ({ page }) => {
  await page.getByRole('button', { name: 'Add user' }).click()
  const add = dialog(page, 'Add a user')
  await add.getByLabel('Name').fill('zed')
  await add.getByLabel('Traffic quota').fill('10')
  await add.getByLabel('Expires').fill('2031-01-01')
  await add.getByLabel('Devices').fill('2')
  await add.getByRole('button', { name: 'Add user' }).click()

  await expect(toast(page, 'zed added')).toBeVisible()
  const share = dialog(page, "Share zed's proxy")
  await expect(share.getByRole('img', { name: 'QR code of the proxy link' })).toBeVisible()
  await expect(share).toContainText('tg://proxy?server=203.0.113.10&port=443&secret=ee')
  await expect(share.getByRole('button', { name: 'Copy tg:// link' })).toBeVisible()
  await expect(share.getByRole('button', { name: 'Copy t.me link' })).toBeVisible()
  await share.getByRole('button', { name: 'Close' }).click()

  await expect(row(page, 'zed')).toContainText('quota 10.0 GB')
  await userAction(page, 'zed', 'Edit limits')
  const edit = dialog(page, 'Limits for zed')
  await expect(edit.getByLabel('Devices')).toHaveValue('2')
  await expect(edit.getByLabel('Traffic quota')).toHaveValue('10')
  await expect(edit.getByLabel('Expires')).toHaveValue('2031-01-01')
})

test('rejects bad and duplicate user names', async ({ page }) => {
  await page.getByRole('button', { name: 'Add user' }).click()
  const add = dialog(page, 'Add a user')
  await add.getByLabel('Name').fill('bad name')
  await add.getByRole('button', { name: 'Add user' }).click()
  await expect(add).toContainText("Use 1–32 letters, digits, '.', '_' or '-'")
  await add.getByLabel('Name').fill('alice')
  await add.getByRole('button', { name: 'Add user' }).click()
  await expect(add).toContainText('a user named alice already exists')
  await add.getByLabel('Name').fill('ok')
  await add.getByLabel('Traffic quota').fill('-5')
  await add.getByRole('button', { name: 'Add user' }).click()
  await expect(add).toContainText('Traffic quota must be a positive number')
})

test('edits and clears limits', async ({ page }) => {
  await userAction(page, 'alice', 'Edit limits')
  const edit = dialog(page, 'Limits for alice')
  await expect(edit.getByLabel('Traffic quota')).toHaveValue('50')
  await expect(edit.getByLabel('Devices')).toHaveValue('3')
  await edit.getByLabel('Traffic quota').fill('')
  await edit.getByLabel('Expires').fill('')
  await edit.getByRole('button', { name: 'Save' }).click()
  await expect(toast(page, 'alice updated')).toBeVisible()
  await expect(row(page, 'alice')).not.toContainText('quota')
  await expect(row(page, 'alice')).toContainText('never')
})

test('disables and enables a user', async ({ page }) => {
  await page.getByRole('switch', { name: 'Disable bob' }).click()
  await expect(toast(page, 'bob disabled')).toBeVisible()
  await expect(row(page, 'bob')).toContainText('disabled')
  await page.getByRole('switch', { name: 'Enable bob' }).click()
  await expect(toast(page, 'bob enabled')).toBeVisible()
  await expect(page.getByRole('switch', { name: 'Disable bob' })).toBeChecked()
})

test('gives a user a new secret', async ({ page }) => {
  const before = await shareLink(page, 'alice')
  await userAction(page, 'alice', 'New secret')
  await dialog(page, 'New secret for alice?').getByRole('button', { name: 'Make new secret' }).click()
  await expect(toast(page, 'alice has a new secret')).toBeVisible()
  const share = dialog(page, "Share alice's proxy")
  await expect(share).toBeVisible()
  const after = (await share.locator('.font-mono').first().textContent()) ?? ''
  expect(after).toMatch(/^tg:\/\/proxy\?/)
  expect(after).not.toBe(before)
})

test('resets usage, which lifts a used-up quota', async ({ page }) => {
  await expect(row(page, 'used-up')).toContainText('over quota')
  await userAction(page, 'used-up', 'Reset usage')
  await dialog(page, "Reset used-up's usage?").getByRole('button', { name: 'Reset usage' }).click()
  await expect(toast(page, "used-up's usage was reset")).toBeVisible()
  await expect(row(page, 'used-up')).toContainText('active')
  await expect(row(page, 'used-up')).toContainText('0 B')
})

test('deletes a user after confirming', async ({ page }) => {
  await userAction(page, 'bob', 'Delete user')
  const confirm = dialog(page, 'Delete bob?')
  await confirm.getByRole('button', { name: 'Cancel' }).click()
  await expect(row(page, 'bob')).toBeVisible()
  await userAction(page, 'bob', 'Delete user')
  await dialog(page, 'Delete bob?').getByRole('button', { name: 'Delete user' }).click()
  await expect(toast(page, 'bob deleted')).toBeVisible()
  await expect(row(page, 'bob')).toHaveCount(0)
})

test('users of a server with its link down can still be managed', async ({ page }) => {
  await openServer(page, AMS)
  await expect(page.getByText('Changes reach the server when its link is back.')).toBeVisible()
  await page.getByRole('switch', { name: 'Disable spare2' }).click()
  await expect(toast(page, 'spare2 disabled')).toBeVisible()
})

test('the actions menu on the last row is not clipped', async ({ page }) => {
  const last = page.getByRole('button', { name: /^Actions for / }).last()
  await last.scrollIntoViewIfNeeded()
  await last.click()
  const menu = page.getByRole('menu')
  await expect(menu).toBeVisible()
  for (const item of await menu.getByRole('menuitem').all()) await expect(item).toBeInViewport()
  const box = (await menu.boundingBox())!
  const viewport = page.viewportSize()!
  expect(box.y).toBeGreaterThanOrEqual(0)
  expect(box.y + box.height).toBeLessThanOrEqual(viewport.height)
  // It closes on Escape and on scroll.
  await page.keyboard.press('Escape')
  await expect(menu).toBeHidden()
  await last.click()
  await page.mouse.wheel(0, -200)
  await expect(page.getByRole('menu')).toBeHidden()
})
