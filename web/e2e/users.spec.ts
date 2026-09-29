import type { Page } from '@playwright/test'
import { dialog, expect, openServer, test, toast } from './fixtures'

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
  await openServer(page, 'fra1')
  await expect(row(page, 'alice')).toBeVisible()
})

test('shows users, their state and totals', async ({ page }) => {
  await expect(page.getByText('Users', { exact: true }).locator('..')).toContainText('22')
  await expect(row(page, 'alice')).toContainText('quota 50.0 GB')
  await expect(row(page, 'old-phone')).toContainText('disabled')
  await expect(row(page, 'expired-trial')).toContainText('expired')
  await expect(row(page, 'guest-wifi')).toContainText('disabled')
  await expect(row(page, 'bob')).toContainText('never')
  await expect(row(page, 'office')).toContainText('online')
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
  await add.getByLabel('Download speed').fill('25')
  await add.getByRole('button', { name: 'Add user' }).click()

  await expect(toast(page, 'zed added')).toBeVisible()
  const share = dialog(page, "Share zed's proxy")
  await expect(share.getByRole('img', { name: 'QR code of the proxy link' })).toBeVisible()
  await expect(share).toContainText('tg://proxy?server=203.0.113.10&port=443&secret=ee')
  await expect(share.getByRole('button', { name: 'Copy t.me link' })).toBeVisible()
  // Frankfurt is dual-stack, so there is a link per address.
  await share.getByRole('button', { name: '2001:db8::10' }).click()
  await expect(share).toContainText('tg://proxy?server=2001:db8::10')
  await share.getByRole('button', { name: 'Close' }).click()

  await expect(row(page, 'zed')).toContainText('quota 10.0 GB')
  await userAction(page, 'zed', 'Edit limits')
  const edit = dialog(page, 'Limits for zed')
  await expect(edit.getByLabel('Devices')).toHaveValue('2')
  await expect(edit.getByLabel('Download speed')).toHaveValue('25')
  await expect(edit.getByLabel('Expires')).toHaveValue('2031-01-01')
})

test('rejects bad and duplicate user names', async ({ page }) => {
  await page.getByRole('button', { name: 'Add user' }).click()
  const add = dialog(page, 'Add a user')
  await add.getByLabel('Name').fill('bad name')
  await add.getByRole('button', { name: 'Add user' }).click()
  await expect(add).toContainText("Use 1–64 letters, digits, '.', '_' or '-'")
  await add.getByLabel('Name').fill('alice')
  await add.getByRole('button', { name: 'Add user' }).click()
  await expect(add).toContainText('User already exists')
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
  await edit.getByLabel('Connections').fill('12')
  await edit.getByRole('button', { name: 'Save' }).click()
  await expect(toast(page, 'alice updated')).toBeVisible()
  await expect(row(page, 'alice')).not.toContainText('quota')
  await expect(row(page, 'alice')).toContainText('/ 12')
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

test('resets a quota', async ({ page }) => {
  await userAction(page, 'alice', 'Reset quota')
  await dialog(page, "Reset alice's quota?").getByRole('button', { name: 'Reset quota' }).click()
  await expect(toast(page, "alice's quota was reset")).toBeVisible()
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

test('changing the disguise domain changes every link', async ({ page }) => {
  const before = await shareLink(page, 'alice')
  expect(before).toContain('7777772e676f6f676c652e636f6d') // www.google.com
  await page.getByRole('button', { name: 'settings' }).click()
  await expect(page).toHaveURL(/#\/servers\/fra1\/settings$/)
  const change = page.getByRole('button', { name: 'Change domain' })
  await expect(change).toBeDisabled()
  await page.getByLabel('New domain').fill('www.cloudflare.com')
  await change.click()
  await expect(toast(page, /Now disguised as www.cloudflare.com/)).toBeVisible()

  await page.getByRole('button', { name: 'users' }).click()
  const after = await shareLink(page, 'alice')
  expect(after).toContain('7777772e636c6f7564666c6172652e636f6d') // www.cloudflare.com
})

test('settings show the server details and router key', async ({ page }) => {
  await openServer(page, 'fra1', 'settings')
  await expect(page.getByText('root@203.0.113.10:22')).toBeVisible()
  await expect(page.getByText('SHA256:p2OiwWbYoEAsmmYWEMy1L12ya7lUDzuHYODmtJ18R/w')).toBeVisible()
  await expect(page.getByText(/^ssh-ed25519 .* nasnet-mtproto-remote$/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Copy key' })).toBeVisible()
})

test('a stopped server says its users cannot be loaded', async ({ page }) => {
  await openServer(page, 'ams1')
  await expect(page.getByText(/port 9091 failed/).first()).toBeVisible()
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
