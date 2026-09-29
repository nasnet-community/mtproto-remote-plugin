import { dialog, expect, openServer, test, toast } from './fixtures'

test('lists every server with its state', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
  const card = (name: string) => page.getByRole('button', { name: new RegExp(name) })
  await expect(card('Frankfurt VPS')).toContainText('active')
  await expect(card('Frankfurt VPS')).toContainText('22 users')
  await expect(card('Frankfurt VPS')).toContainText('v3.5.9')
  await expect(card('Helsinki ARM')).toContainText('hel.example.net:8443')
  await expect(card('Amsterdam backup')).toContainText('inactive')
  await expect(card('Istanbul test')).toContainText('failed')
  await expect(card('Old Hetzner')).toContainText('unreachable')
  await expect(card('Old Hetzner')).toContainText('timed out connecting')
  await expect(card('New York')).toContainText('unreachable')
})

test('opens a server from its card and comes back', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('button', { name: /Helsinki ARM/ }).click()
  await expect(page).toHaveURL(/#\/servers\/hel1$/)
  await expect(page.getByRole('heading', { name: 'Helsinki ARM' })).toBeVisible()
  await expect(page.getByText('deploy@hel.example.net:2222')).toBeVisible()
  await page.getByRole('button', { name: 'All servers' }).click()
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
})

test('stops and starts the service', async ({ page }) => {
  await openServer(page, 'fra1')
  const header = page.locator('main')
  await page.getByRole('button', { name: 'Stop' }).click()
  await expect(toast(page, 'Proxy stopped')).toBeVisible()
  await expect(header.getByText('inactive', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Start' }).click()
  await expect(toast(page, 'Proxy started')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Stop' })).toBeVisible()
  await page.getByRole('button', { name: 'Restart' }).click()
  await expect(toast(page, 'Proxy restarted')).toBeVisible()
})

test('shows the logs of a running and of a crashed server', async ({ page }) => {
  await openServer(page, 'fra1')
  await page.getByRole('button', { name: 'Logs' }).click()
  await expect(dialog(page, 'Logs — Frankfurt VPS')).toContainText('handshake ok user=alice')
  await page.keyboard.press('Escape')
  await expect(dialog(page, /^Logs —/)).toBeHidden()

  await openServer(page, 'ist1')
  await expect(page.locator('main').getByText('failed', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Logs' }).click()
  await expect(dialog(page, /^Logs —/)).toContainText('Address already in use')
})

test('updates the proxy', async ({ page }) => {
  await openServer(page, 'fra1')
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Update proxy' }).click()
  await expect(toast(page, 'Updated to version 3.6.0')).toBeVisible({ timeout: 10000 })
  await expect(page.getByText('version 3.6.0')).toBeVisible()
})

test('renames a server', async ({ page }) => {
  await openServer(page, 'fra1')
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Rename' }).click()
  const rename = dialog(page, 'Rename server')
  await rename.getByLabel('Name').fill('Frankfurt main')
  await rename.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByRole('heading', { name: 'Frankfurt main' })).toBeVisible()
})

test('uninstalls and removes a server', async ({ page }) => {
  await openServer(page, 'ams1')
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Remove server' }).click()
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await expect(remove.getByRole('radio', { name: /Uninstall the proxy/ })).toBeChecked()
  await remove.getByRole('button', { name: 'Uninstall and remove' }).click()
  await expect(toast(page, 'Amsterdam backup removed')).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
  await expect(page.getByRole('button', { name: /Amsterdam backup/ })).toHaveCount(0)
})

test('an unreachable server can only be forgotten, with a warning', async ({ page }) => {
  await openServer(page, 'old1')
  await expect(page.getByText('timed out connecting to 192.0.2.44:22').first()).toBeVisible()
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Remove server' }).click()
  const remove = dialog(page, 'Remove Old Hetzner?')
  await remove.getByRole('button', { name: 'Uninstall and remove' }).click()
  await expect(remove).toContainText('timed out connecting')

  await remove.getByRole('radio', { name: /Only remove from this list/ }).check()
  await remove.getByRole('button', { name: 'Remove from list' }).click()
  await expect(toast(page, /could not be cleaned up/)).toBeVisible()
  await expect(page.getByRole('button', { name: /Old Hetzner/ })).toHaveCount(0)
})

test('explains a revoked router key', async ({ page }) => {
  await openServer(page, 'nyc1')
  await expect(page.getByText(/no longer accepts the router key/).first()).toBeVisible()
  await expect(page.getByText('Add the server again to give the router a new key.')).toBeVisible()
})

test('says so when a server is not in the list', async ({ page }) => {
  await page.goto('/#/servers/nope')
  await expect(page.getByText('That server is not in the list any more.')).toBeVisible()
})

test('switches the theme and remembers it', async ({ page }) => {
  await page.goto('/')
  const html = page.locator('html')
  const before = await html.getAttribute('data-theme')
  await page.getByRole('button', { name: 'Toggle light or dark theme' }).click()
  const after = before === 'dark' ? 'light' : 'dark'
  await expect(html).toHaveAttribute('data-theme', after)
  await page.reload()
  await expect(html).toHaveAttribute('data-theme', after)
})
