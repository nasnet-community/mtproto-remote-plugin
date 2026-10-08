import type { Page } from '@playwright/test'
import { AMS, FRA, HEL, NEW_KEY, PAGE, dialog, expect, openServer, stateHolds, test, toast } from './fixtures'

const card = (page: Page, name: string) => page.getByRole('button', { name: new RegExp(name) })

async function action(page: Page, name: string) {
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name }).click()
}

test('lists every server with its link state', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
  await expect(card(page, 'Frankfurt VPS')).toContainText('link up')
  await expect(card(page, 'Frankfurt VPS')).toContainText('22 users')
  await expect(card(page, 'Frankfurt VPS')).toContainText('agent 0.5.0')
  await expect(card(page, 'Helsinki ARM')).toContainText('link degraded')
  await expect(card(page, 'Helsinki ARM')).toContainText('agent 0.4.2')
  await expect(card(page, 'Helsinki ARM')).toContainText('proxy keeps crashing')
  await expect(card(page, 'Amsterdam backup')).toContainText('link down')
  await expect(card(page, 'Amsterdam backup')).toContainText('last seen 3 hours ago')
})

test('shows the server cap and why it is reached', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('3 of 3 servers')).toBeVisible()
  await expect(page.getByText('This router runs at most 3 servers.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add server' })).toBeDisabled()
})

test('a small router is capped at one server', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { maxServers: 1 } })
  await page.goto('/')
  await expect(page.getByText(/Small routers run one server/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add server' })).toBeDisabled()
})

test('opens a server from its card and comes back', async ({ page }) => {
  await page.goto('/')
  await card(page, 'Helsinki ARM').click()
  await expect(page).toHaveURL(/#\/servers\/2$/)
  await expect(page.getByRole('heading', { name: 'Helsinki ARM' })).toBeVisible()
  await expect(page.getByText('hel.example.net', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'All servers' }).click()
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
})

test('shows the agent status, link connections and events', async ({ page }) => {
  await openServer(page, FRA, 'status')
  const main = page.locator('main')
  await expect(main.getByText('Memory', { exact: true })).toBeVisible()
  await expect(main.getByRole('row', { name: /tcp-1/ })).toContainText('38 ms')
  await expect(main.getByRole('row', { name: /tcp-2/ })).toContainText('up')
  await expect(main).toContainText('Capture rules')
  await expect(main).toContainText('Could not reach Telegram')

  await openServer(page, HEL, 'status')
  await expect(main.getByRole('row', { name: /tcp-2/ })).toContainText('i/o timeout')
  await expect(main).toContainText('Proxy keeps crashing')
  await expect(main).toContainText('Refused a destination outside the allowlist')
})

test('restarts a crashing proxy', async ({ page }) => {
  await openServer(page, HEL)
  await expect(page.getByText(/The proxy keeps crashing/)).toBeVisible()
  await page.getByRole('button', { name: 'Restart proxy' }).click()
  await expect(toast(page, 'Proxy restarted')).toBeVisible()
  await expect(page.getByText(/The proxy keeps crashing/)).toBeHidden()
})

test('updates the agent and shows the steps', async ({ page }) => {
  await openServer(page, HEL)
  await expect(page.locator('main').getByText('agent 0.4.2')).toBeVisible()
  await action(page, 'Update agent')
  const job = dialog(page, /Updating the agent|Agent updated/)
  await expect(job).toContainText('Send the new version')
  await expect(dialog(page, 'Agent updated')).toBeVisible({ timeout: 10000 })
  await expect(job).toContainText('Confirm the update')
  await expect(toast(page, 'Agent updated')).toBeVisible()
  await job.getByRole('button', { name: 'Done' }).click()
  await expect(page.locator('main').getByText('agent 0.5.0')).toBeVisible()
})

test('updates the proxy', async ({ page }) => {
  await openServer(page, HEL)
  await action(page, 'Update proxy')
  await expect(dialog(page, 'Proxy updated')).toBeVisible({ timeout: 10000 })
  await expect(toast(page, 'Proxy updated')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('main').getByText('proxy 3.4.25')).toBeVisible()
})

test('rotates the link keys', async ({ page }) => {
  await openServer(page, FRA)
  await action(page, 'Rotate keys')
  await dialog(page, 'Rotate the link keys?').getByRole('button', { name: 'Rotate keys' }).click()
  await expect(dialog(page, 'Keys rotated')).toBeVisible({ timeout: 10000 })
  await expect(dialog(page, 'Keys rotated')).toContainText('Reconnect with the new keys')
  await expect(toast(page, 'Keys rotated')).toBeVisible()
})

test('repairs a server over SSH and brings its link back', async ({ page }) => {
  await openServer(page, AMS)
  await expect(page.getByText(/The router cannot reach this server's agent/)).toBeVisible()
  await page.getByRole('button', { name: 'Repair' }).click()
  const login = dialog(page, 'Repair Amsterdam backup')
  await expect(login.getByLabel('SSH port')).toHaveValue('22')
  await expect(login.getByLabel('Username')).toHaveValue('root')
  await login.getByLabel('Password').fill('wrong')
  await login.getByRole('button', { name: 'Log in and repair' }).click()
  const failed = dialog(page, 'Repair failed')
  await expect(failed).toContainText('unable to authenticate')
  await expect(toast(page, 'Repair failed')).toBeVisible()
  await failed.getByRole('button', { name: 'Done' }).click()

  await page.getByRole('button', { name: 'Repair' }).click()
  await dialog(page, 'Repair Amsterdam backup').getByLabel('Password').fill('hunter2')
  await dialog(page, 'Repair Amsterdam backup').getByRole('button', { name: 'Log in and repair' }).click()
  await expect(dialog(page, 'Server repaired')).toBeVisible({ timeout: 15000 })
  await expect(toast(page, 'Amsterdam backup repaired')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('main').getByText('link up', { exact: true })).toBeVisible()
})

test('renames a server', async ({ page }) => {
  await openServer(page, FRA)
  await action(page, 'Rename')
  const rename = dialog(page, 'Rename server')
  await rename.getByLabel('Name').fill('Frankfurt main')
  await rename.getByRole('button', { name: 'Save' }).click()
  await expect(toast(page, 'Renamed to Frankfurt main')).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Frankfurt main' })).toBeVisible()
})

test('removes a server over its link', async ({ page }) => {
  await openServer(page, FRA)
  await action(page, 'Remove server')
  await dialog(page, 'Remove Frankfurt VPS?').getByRole('button', { name: 'Remove server' }).click()
  await expect(toast(page, 'Frankfurt VPS removed')).toBeVisible({ timeout: 10000 })
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
  await expect(card(page, 'Frankfurt VPS')).toHaveCount(0)
})

test('a server with its link down needs a login to be removed', async ({ page }) => {
  await openServer(page, AMS)
  await action(page, 'Remove server')
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await remove.getByRole('button', { name: 'Remove server' }).click()
  await expect(remove).toContainText('log in over SSH')
  await expect(remove.getByLabel('Username')).toHaveValue('root')
  await remove.getByLabel('Password').fill('hunter2')
  await remove.getByRole('button', { name: 'Log in and remove' }).click()
  await expect(toast(page, 'Amsterdam backup removed')).toBeVisible({ timeout: 10000 })
  await expect(card(page, 'Amsterdam backup')).toHaveCount(0)
})

test('a server with its link down can be forgotten without a login', async ({ page }) => {
  await openServer(page, AMS)
  await action(page, 'Remove server')
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await remove.getByRole('button', { name: 'Remove server' }).click()
  await remove.getByRole('button', { name: 'Forget without removing' }).click()
  await expect(toast(page, 'Amsterdam backup removed')).toBeVisible({ timeout: 10000 })
  await expect(card(page, 'Amsterdam backup')).toHaveCount(0)
})

test('shows the logs, and says why a down server has none', async ({ page }) => {
  await openServer(page, FRA, 'logs')
  await expect(page.locator('pre')).toContainText('handshake ok user=alice')
  await openServer(page, HEL, 'logs')
  await expect(page.locator('pre')).toContainText('address already in use')
  await openServer(page, AMS, 'logs')
  await expect(page.getByText('the link to this server is down')).toBeVisible()
})

test('says so when a server is not in the list', async ({ page }) => {
  await page.goto('/#/servers/99')
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

test('a job closed while it runs still reports how it ended', async ({ page }) => {
  await openServer(page, HEL)
  await action(page, 'Update agent')
  await expect(dialog(page, 'Updating the agent')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog(page, /Updating the agent|Agent updated/)).toBeHidden()
  await expect(toast(page, 'Agent updated')).toBeVisible({ timeout: 10000 })
  await expect(page.locator('main').getByText('agent 0.5.0')).toBeVisible()
})

test('a removal is followed to its end even after the server is gone', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { slowRemove: true } })
  await openServer(page, FRA)
  await action(page, 'Remove server')
  await dialog(page, 'Remove Frankfurt VPS?').getByRole('button', { name: 'Remove server' }).click()
  await expect(page.getByText('Forget the server')).toBeVisible({ timeout: 10000 })
  // the page's 5 s poll finds the server gone before the job's last event
  await page.waitForTimeout(5500)
  await expect(page.getByText('That server is not in the list any more.')).toHaveCount(0)
  await expect(toast(page, 'Frankfurt VPS removed')).toBeVisible({ timeout: 10000 })
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
})

test('the SSH password is dropped once a removal starts', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { slowRemove: true } })
  await openServer(page, AMS)
  await action(page, 'Remove server')
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await remove.getByRole('button', { name: 'Remove server' }).click()
  await remove.getByLabel('Password').fill('hunter2-secret')
  expect(await stateHolds(page, 'hunter2-secret')).toBe(true) // the check can see it
  await remove.getByRole('button', { name: 'Log in and remove' }).click()
  await expect(page.getByText('Forget the server')).toBeVisible({ timeout: 10000 })
  expect(await stateHolds(page, 'hunter2-secret')).toBe(false)
})

test('the logo goes home without reloading the page', async ({ page }) => {
  // like the panel's view without a trailing slash: the base tag points one level down
  await page.goto('/__panel-view#/servers/1')
  await expect(page.getByRole('heading', { name: 'Frankfurt VPS' })).toBeVisible()
  await page.evaluate(() => ((window as unknown as { kept: boolean }).kept = true))
  await page.getByRole('link', { name: 'MTProto Remote' }).click()
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
  expect(new URL(page.url()).pathname).toBe('/__panel-view')
  expect(await page.evaluate(() => (window as unknown as { kept?: boolean }).kept)).toBe(true)
})

/** The host keys sent with each repair or removal. */
function sentKeys(page: Page) {
  const keys: string[] = []
  page.on('request', (r) => {
    if (r.method() !== 'POST' || !/\/(repair|remove)$/.test(r.url())) return
    const b = JSON.parse(r.postData() ?? '{}')
    keys.push(b.hostKey ?? b.login?.hostKey ?? '')
  })
  return keys
}

test('a rebuilt server is repaired once its new host key is confirmed', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { rebuilt: true } })
  const keys = sentKeys(page)
  await openServer(page, AMS)
  await page.getByRole('button', { name: 'Repair' }).click()
  const repair = dialog(page, 'Repair Amsterdam backup')
  await repair.getByLabel('Password').fill('hunter2')
  await repair.getByRole('button', { name: 'Log in and repair' }).click()
  await expect(repair).toContainText('shows a different key than last time')
  await expect(repair).toContainText(NEW_KEY)
  const trust = repair.getByRole('button', { name: 'Trust and repair' })
  await expect(trust).toBeDisabled()
  await repair.getByLabel('I know why the key changed').check()
  await trust.click()
  await expect(dialog(page, 'Server repaired')).toBeVisible({ timeout: 15000 })
  await expect(toast(page, 'Amsterdam backup repaired')).toBeVisible()
  expect(keys).toEqual([NEW_KEY])

  // the new key is pinned now, so there is no question the next time
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'Repair' }).click()
  await dialog(page, 'Repair Amsterdam backup').getByLabel('Password').fill('hunter2')
  await dialog(page, 'Repair Amsterdam backup').getByRole('button', { name: 'Log in and repair' }).click()
  await expect(dialog(page, 'Server repaired')).toBeVisible({ timeout: 15000 })
  expect(keys).toEqual([NEW_KEY, NEW_KEY])
})

test('going back from the key question keeps the login for another try', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { rebuilt: true } })
  await openServer(page, AMS)
  await page.getByRole('button', { name: 'Repair' }).click()
  const repair = dialog(page, 'Repair Amsterdam backup')
  await repair.getByLabel('Password').fill('hunter2')
  await repair.getByRole('button', { name: 'Log in and repair' }).click()
  await repair.getByRole('button', { name: 'Back' }).click()
  await expect(repair.getByLabel('Password')).toHaveValue('hunter2')
  await repair.getByRole('button', { name: 'Cancel' }).click()
  expect(await stateHolds(page, 'hunter2')).toBe(false)
})

test('a rebuilt server with its link down is removed once its new key is confirmed', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { rebuilt: true } })
  const keys = sentKeys(page)
  await openServer(page, AMS)
  await action(page, 'Remove server')
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await remove.getByRole('button', { name: 'Remove server' }).click()
  await remove.getByLabel('Password').fill('hunter2')
  await remove.getByRole('button', { name: 'Log in and remove' }).click()
  await expect(remove).toContainText('shows a different key than last time')
  await expect(remove).toContainText(NEW_KEY)
  const trust = remove.getByRole('button', { name: 'Trust and remove' })
  await expect(trust).toBeDisabled()
  await remove.getByLabel('I know why the key changed').check()
  await trust.click()
  await expect(toast(page, 'Amsterdam backup removed')).toBeVisible({ timeout: 10000 })
  await expect(card(page, 'Amsterdam backup')).toHaveCount(0)
  expect(keys).toEqual(['', NEW_KEY]) // the first try had no login
})

test('a server whose install never finished can only be removed', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { halfInstalled: true } })
  await openServer(page, AMS)
  await expect(page.getByText(/install never finished/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Repair' })).toHaveCount(0)
  await action(page, 'Remove server')
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await remove.getByRole('button', { name: 'Remove server' }).click()
  await expect(remove.getByLabel('SSH port')).toHaveValue('22')
  await expect(remove.getByLabel('Username')).toHaveValue('root')
  await remove.getByLabel('Password').fill('hunter2')
  await remove.getByRole('button', { name: 'Log in and remove' }).click()
  // nothing was pinned, so this is a first connection
  await expect(remove).toContainText('First connection to 198.51.100.77')
  await expect(remove).toContainText(NEW_KEY)
  await remove.getByRole('button', { name: 'Trust and remove' }).click()
  await expect(toast(page, 'Amsterdam backup removed')).toBeVisible({ timeout: 10000 })
})

test("every API call carries the page's header", async ({ page, request }) => {
  const missing: string[] = []
  page.on('request', (r) => {
    const path = new URL(r.url()).pathname
    if (path.startsWith('/api/') && !path.endsWith('/stream') && r.headers()['x-mtproto-ui'] !== '1') missing.push(path)
  })
  await openServer(page, FRA, 'status')
  await expect(page.locator('main').getByText('Memory', { exact: true })).toBeVisible()
  await openServer(page, FRA)
  await expect(page.getByRole('row', { name: /alice/ })).toBeVisible()
  expect(missing).toEqual([])
  // and a call without it is refused, as the plugin does
  expect((await request.get('/api/servers')).status()).toBe(403)
  expect((await request.get('/api/servers', { headers: PAGE })).status()).toBe(200)
})
