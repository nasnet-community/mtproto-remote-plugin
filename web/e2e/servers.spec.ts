import type { Page } from '@playwright/test'
import { AMS, FRA, HEL, NEW_KEY, PAGE, alertDialog, dialog, expect, openServer, stateHolds, test, toast } from './fixtures'

const card = (page: Page, name: string) => page.getByRole('button', { name: new RegExp(name) })
const actions = (page: Page) => page.getByRole('group', { name: 'Server actions' })
const attention = (page: Page) => page.getByRole('region', { name: 'Needs attention' })

async function action(page: Page, name: string) {
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name }).click()
}

test('server cards show health with its reason, who is online, today and updates', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
  const fra = card(page, 'Frankfurt VPS')
  await expect(fra).toContainText('Working')
  await expect(fra).toContainText('6 of 22 online')
  await expect(fra).toContainText(/[\d.]+ [KMG]?B today/)
  await expect(fra).toContainText('agent 0.5.0 · proxy 3.4.25')
  await expect(fra).not.toContainText('Update available')
  const hel = card(page, 'Helsinki ARM')
  await expect(hel).toContainText('Needs attention')
  await expect(hel).toContainText('The proxy keeps crashing')
  // its agent is too old to say who is online
  await expect(hel).toContainText('5 users')
  await expect(hel).toContainText('Update available')
  const ams = card(page, 'Amsterdam backup')
  await expect(ams).toContainText('Unreachable')
  await expect(ams).toContainText(/Not reachable since .+ \(3 hours ago\)/)
  // the agent can't say who is online while it is unreachable
  await expect(ams).toContainText('2 users')
  await expect(ams).toContainText('Update available')
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

test('the status tab says in plain words how the server is doing', async ({ page }) => {
  await openServer(page, FRA, 'status')
  const main = page.locator('main')
  await expect(main.getByRole('heading', { name: 'Everything is working' })).toBeVisible()
  await expect(main.getByText(/Last report \d+ s ago/)).toBeVisible()
  await expect(main.getByRole('listitem').filter({ hasText: 'Proxy' }).first()).toContainText('running')
  await expect(main.getByRole('heading', { name: 'Traffic, last 15 minutes' })).toBeVisible()
  await expect(main.getByRole('img', { name: /From Telegram .+ now/ })).toBeVisible()
  await expect(main.getByText('Memory', { exact: true })).toBeVisible()
  await expect(main.getByText('Connections carried', { exact: true })).toBeVisible()
  await expect(main.getByText('Telegram connections now', { exact: true })).toBeVisible()
  await expect(main.getByRole('row', { name: /tcp-1/ })).toContainText('38 ms')
  await expect(main.getByRole('row', { name: /tcp-2/ })).toContainText('up')
  await expect(main).toContainText("Couldn't reach Telegram")
  // a bad number gets a hint that says what it means
  await expect(main).toContainText('A few now and then is normal.')

  await openServer(page, HEL, 'status')
  await expect(main.getByRole('heading', { name: "Users can't connect right now" })).toBeVisible()
  await expect(main.getByRole('listitem').filter({ hasText: 'Proxy' }).first()).toContainText('keeps crashing')
  await expect(main.getByRole('listitem').filter({ hasText: 'Link' }).first()).toContainText('1 of 2 up')
  await expect(main.getByRole('listitem').filter({ hasText: 'Cover site' })).toContainText('not checked yet')
  await expect(main.getByText(/Proxy stopped \d\d:\d\d/)).toBeVisible()
  await expect(main).toContainText('It restarts on its own and waits longer each time.')
  await expect(main).toContainText('Something on the server tried to reach a non-Telegram address; the router refused it.')
  await expect(main.getByRole('row', { name: /tcp-2/ })).toContainText('i/o timeout')
  await expect(main).toContainText('Proxy keeps crashing')
  await expect(main).toContainText('Refused a non-Telegram address')
})

test('restarting a crashing proxy asks first, then says "Restarting…" until it is back', async ({ page }) => {
  await openServer(page, HEL)
  await expect(attention(page)).toContainText('The proxy keeps crashing')
  await expect(attention(page)).toContainText('Port 443 is taken by another program on the server. From the log')
  await attention(page).getByRole('button', { name: 'Restart proxy' }).click()
  const confirm = alertDialog(page, 'Restart the proxy on Helsinki ARM?')
  await expect(confirm).toContainText('open connections right now, so nobody is cut off.')
  await expect(confirm).toContainText('The button shows "Restarting…" until the proxy reports back.')
  await confirm.getByRole('button', { name: 'Restart proxy' }).click()
  await expect(actions(page).getByRole('button', { name: 'Restarting…' })).toBeDisabled()
  await expect(toast(page, 'Proxy restarted')).toBeVisible({ timeout: 10000 })
  await expect(actions(page).getByRole('button', { name: 'Restart proxy' })).toBeEnabled()
  await expect(attention(page).getByText('The proxy keeps crashing')).toHaveCount(0)
})

test('restarting counts the connections that drop', async ({ page }) => {
  await openServer(page, FRA)
  await actions(page).getByRole('button', { name: 'Restart proxy' }).click()
  const confirm = alertDialog(page, 'Restart the proxy on Frankfurt VPS?')
  await expect(confirm).toContainText(/\d+open connections drop, from 6 people online\. Telegram reconnects on its own/)
  await confirm.getByRole('button', { name: 'Cancel' }).click()
  await expect(confirm).toBeHidden()
})

test('a restart that never comes back ends with an error after a minute', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { restartHangs: true } })
  await page.clock.install()
  await openServer(page, FRA)
  await actions(page).getByRole('button', { name: 'Restart proxy' }).click()
  await alertDialog(page, 'Restart the proxy on Frankfurt VPS?').getByRole('button', { name: 'Restart proxy' }).click()
  await expect(actions(page).getByRole('button', { name: 'Restarting…' })).toBeVisible()
  await page.clock.fastForward(61_000)
  const err = toast(page, "The proxy isn't back after a minute.")
  await expect(err).toBeVisible()
  await err.getByRole('button', { name: 'View logs' }).click()
  await expect(page).toHaveURL(/#\/servers\/1\/logs$/)
})

test('stops the proxy after confirming, and starts it again', async ({ page }) => {
  await openServer(page, FRA)
  await actions(page).getByRole('button', { name: 'Stop proxy' }).click()
  const confirm = alertDialog(page, 'Stop the proxy on Frankfurt VPS?')
  await expect(confirm).toContainText('open connections drop, from 6 people online.')
  await expect(confirm).toContainText('It stays off, across restarts and repairs, until you press Start proxy.')
  await confirm.getByRole('button', { name: 'Stop proxy' }).click()
  await expect(toast(page, 'Proxy stopped')).toBeVisible()
  await expect(attention(page)).toContainText('You stopped the proxy')
  await expect(actions(page).getByRole('button', { name: 'Restart proxy' })).toBeDisabled()
  await attention(page).getByRole('button', { name: 'Start proxy' }).click()
  await expect(toast(page, 'Proxy started')).toBeVisible()
  await expect(attention(page)).toHaveCount(0, { timeout: 10000 })
  await expect(actions(page).getByRole('button', { name: 'Stop proxy' })).toBeVisible()
})

test('updates are offered only when one is due', async ({ page }) => {
  await openServer(page, FRA)
  await page.getByRole('button', { name: 'More actions' }).click()
  await expect(page.getByRole('menuitem', { name: /Update/ })).toHaveCount(0)
  await page.keyboard.press('Escape')
  await expect(page.getByRole('button', { name: /^Update/ })).toHaveCount(0)

  await openServer(page, HEL)
  await expect(page.getByRole('button', { name: 'Update to 0.5.0' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Update to 3.4.25' })).toBeVisible()
  await page.getByRole('button', { name: 'More actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Update agent to 0.5.0' })).toBeVisible()
  await expect(page.getByRole('menuitem', { name: 'Update proxy to 3.4.25' })).toBeVisible()
})

test('updates the agent after confirming, and shows the steps', async ({ page }) => {
  await openServer(page, HEL)
  await expect(page.locator('main').getByText('agent 0.4.2')).toBeVisible()
  await page.getByRole('button', { name: 'Update to 0.5.0' }).click()
  const confirm = alertDialog(page, 'Update the agent on Helsinki ARM to 0.5.0?')
  await expect(confirm).toContainText('open connections right now')
  await confirm.getByRole('button', { name: 'Update agent' }).click()
  const job = dialog(page, /Updating the agent|Agent updated/)
  await expect(job).toContainText('Send the new version')
  await expect(job).toContainText(/Step \d of 4/)
  await expect(dialog(page, 'Agent updated')).toBeVisible({ timeout: 10000 })
  await expect(job).toContainText('Confirm the update')
  await expect(toast(page, 'Agent updated')).toBeVisible()
  await job.getByRole('button', { name: 'Done' }).click()
  await expect(page.locator('main').getByText('agent 0.5.0')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Update to 0.5.0' })).toHaveCount(0)
})

test('updates the proxy from the menu', async ({ page }) => {
  await openServer(page, HEL)
  await action(page, 'Update proxy to 3.4.25')
  await alertDialog(page, 'Update the proxy on Helsinki ARM to 3.4.25?').getByRole('button', { name: 'Update proxy' }).click()
  await expect(dialog(page, 'Proxy updated')).toBeVisible({ timeout: 10000 })
  await expect(toast(page, 'Proxy updated')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.locator('main').getByText('proxy 3.4.25')).toBeVisible()
})

test('renews the link keys, and says users keep their links', async ({ page }) => {
  await openServer(page, FRA)
  await page.getByRole('button', { name: 'More actions' }).click()
  await expect(page.getByRole('menuitem', { name: 'Renew link keys' })).toContainText('Users keep their links')
  await page.getByRole('menuitem', { name: 'Renew link keys' }).click()
  await alertDialog(page, 'Renew the link keys?').getByRole('button', { name: 'Renew link keys' }).click()
  await expect(dialog(page, 'Link keys renewed')).toBeVisible({ timeout: 10000 })
  await expect(dialog(page, 'Link keys renewed')).toContainText('Reconnect with the new keys')
  await expect(toast(page, 'Link keys renewed')).toBeVisible()
})

test('what needs the link is greyed out, with the reason, while it is down', async ({ page }) => {
  await openServer(page, AMS)
  await expect(attention(page)).toContainText("The router can't reach this server")
  await expect(attention(page)).toContainText('Check that the server is on and its address is on the domestic list, then run Repair.')
  await expect(actions(page).getByRole('button', { name: 'Restart proxy' })).toBeDisabled()
  await expect(actions(page).getByRole('button', { name: 'Restart proxy' })).toHaveAttribute('title', 'Needs the link to this server')
  await expect(actions(page).getByRole('button', { name: 'Stop proxy' })).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Update to 3.4.25' })).toBeDisabled()
  await page.getByRole('button', { name: 'More actions' }).click()
  const update = page.getByRole('menuitem', { name: /Update proxy to 3.4.25/ })
  await expect(update).toHaveAttribute('aria-disabled', 'true')
  await expect(update).toContainText('Needs the link to this server')
  await expect(page.getByRole('menuitem', { name: /Renew link keys/ })).toContainText('Users keep their links. Needs the link to this server')
  await update.click({ force: true })
  // nothing happens: no confirmation, and the menu stays open
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  await expect(page.getByRole('menu')).toBeVisible()
  await page.keyboard.press('Escape')
  await page.getByRole('tab', { name: 'Settings' }).click()
  await expect(page.getByRole('button', { name: 'Change cover site…' })).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Change link port…' })).toBeDisabled()
})

test('the attention list offers the fix: Open Status, View logs', async ({ page }) => {
  await openServer(page, HEL)
  await expect(attention(page)).toContainText('1 of 2 link connections are up')
  await expect(attention(page)).toContainText('tcp-2 timed out 2 hours ago. Users still get through on the other one.')
  await attention(page).getByRole('button', { name: 'Open Status' }).click()
  await expect(page).toHaveURL(/#\/servers\/2\/status$/)
  await expect(attention(page).getByRole('button', { name: 'Open Status' })).toHaveCount(0)
  await attention(page).getByRole('button', { name: 'View logs' }).click()
  await expect(page).toHaveURL(/#\/servers\/2\/logs$/)
  await expect(attention(page).getByRole('button', { name: 'View logs' })).toHaveCount(0)
})

test('repairs a server over SSH and brings its link back', async ({ page }) => {
  await openServer(page, AMS)
  await attention(page).getByRole('button', { name: 'Repair' }).click()
  const login = dialog(page, 'Repair Amsterdam backup')
  await expect(login.getByLabel('SSH port')).toHaveValue('22')
  await expect(login.getByLabel('Username')).toHaveValue('root')
  await login.getByLabel('Password').fill('wrong')
  await login.getByRole('button', { name: 'Log in and repair' }).click()
  const failed = dialog(page, 'Repair failed')
  await expect(failed).toContainText('unable to authenticate')
  await expect(toast(page, 'Repair failed')).toBeVisible()
  await failed.getByRole('button', { name: 'Done' }).click()

  await actions(page).getByRole('button', { name: 'Repair' }).click()
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
  const done = toast(page, 'Renamed to Frankfurt main')
  await expect(done).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Frankfurt main' })).toBeVisible()
  // desktops: toasts sit top right, under the header, off the list
  const box = (await done.boundingBox())!
  expect(box.y).toBeGreaterThanOrEqual(56)
  expect(box.y).toBeLessThan(120)
  expect(box.x + box.width).toBeGreaterThan(page.viewportSize()!.width - 40)
})

test('removes a server over its link', async ({ page }) => {
  await openServer(page, FRA)
  await action(page, 'Remove server…')
  await dialog(page, 'Remove Frankfurt VPS?').getByRole('button', { name: 'Remove server' }).click()
  await expect(toast(page, 'Frankfurt VPS removed')).toBeVisible({ timeout: 10000 })
  await expect(page.getByRole('heading', { name: 'Servers' })).toBeVisible()
  await expect(card(page, 'Frankfurt VPS')).toHaveCount(0)
})

test('a server with its link down needs a login to be removed', async ({ page }) => {
  await openServer(page, AMS)
  await action(page, 'Remove server…')
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
  await action(page, 'Remove server…')
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await remove.getByRole('button', { name: 'Remove server' }).click()
  await remove.getByRole('button', { name: 'Forget without removing' }).click()
  await expect(toast(page, 'Amsterdam backup removed')).toBeVisible({ timeout: 10000 })
  await expect(card(page, 'Amsterdam backup')).toHaveCount(0)
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

test('a job closed while it runs stays in the job tray and still reports how it ended', async ({ page }) => {
  await openServer(page, HEL)
  await page.getByRole('button', { name: 'Update to 0.5.0' }).click()
  await alertDialog(page, /Update the agent/).getByRole('button', { name: 'Update agent' }).click()
  const job = dialog(page, /Updating the agent/)
  await expect(job).toContainText('You can close this. It keeps going')
  await job.getByRole('button', { name: 'Close', exact: true }).click()
  await expect(dialog(page, /Updating the agent|Agent updated/)).toBeHidden()
  const tray = page.getByRole('button', { name: 'Updating the agent · Helsinki ARM' })
  await expect(tray).toBeVisible()
  await tray.click()
  const panel = page.getByRole('region', { name: 'Jobs' })
  await expect(panel).toContainText(/Helsinki ARM · step \d of 4/)
  await panel.getByRole('button', { name: 'Open' }).first().click()
  await expect(dialog(page, /Updating the agent|Agent updated/)).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(toast(page, 'Agent updated')).toBeVisible({ timeout: 10000 })
  await expect(page.locator('main').getByText('agent 0.5.0')).toBeVisible()
  // and it lands among the recent results
  await page.getByRole('button', { name: 'Recent jobs' }).click()
  await expect(page.getByRole('region', { name: 'Jobs' })).toContainText('Agent updated · Helsinki ARM')
})

test('a removal is followed to its end even after the server is gone', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { slowRemove: true } })
  await openServer(page, FRA)
  await action(page, 'Remove server…')
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
  await action(page, 'Remove server…')
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
  await page.getByRole('link', { name: 'Remote MTProto' }).click()
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
  await actions(page).getByRole('button', { name: 'Repair' }).click()
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
  await actions(page).getByRole('button', { name: 'Repair' }).click()
  await dialog(page, 'Repair Amsterdam backup').getByLabel('Password').fill('hunter2')
  await dialog(page, 'Repair Amsterdam backup').getByRole('button', { name: 'Log in and repair' }).click()
  await expect(dialog(page, 'Server repaired')).toBeVisible({ timeout: 15000 })
  expect(keys).toEqual([NEW_KEY, NEW_KEY])
})

test('going back from the key question keeps the login for another try', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { rebuilt: true } })
  await openServer(page, AMS)
  await actions(page).getByRole('button', { name: 'Repair' }).click()
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
  await action(page, 'Remove server…')
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

test('a server whose install never finished can only be removed, trusting its first key', async ({ page, request }) => {
  await request.post('/api/__reset', { data: { halfInstalled: true } })
  const keys = sentKeys(page)
  await openServer(page, AMS)
  await expect(attention(page)).toContainText("This server's install never finished")
  await expect(page.getByRole('button', { name: 'Repair' })).toHaveCount(0)
  await attention(page).getByRole('button', { name: 'Remove server…' }).click()
  const remove = dialog(page, 'Remove Amsterdam backup?')
  await remove.getByRole('button', { name: 'Remove server' }).click()
  await expect(remove.getByLabel('SSH port')).toHaveValue('22')
  await expect(remove.getByLabel('Username')).toHaveValue('root')
  await remove.getByLabel('Password').fill('hunter2')
  await remove.getByRole('button', { name: 'Log in and remove' }).click()
  // nothing was pinned, so this is a first connection: its key is trusted as it is
  await expect(toast(page, 'Amsterdam backup removed')).toBeVisible({ timeout: 10000 })
  expect(keys).toEqual(['', NEW_KEY])
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

test('tabs are real tabs: selected state and arrow keys', async ({ page }) => {
  await openServer(page, FRA)
  const users = page.getByRole('tab', { name: 'Users' })
  await expect(users).toHaveAttribute('aria-selected', 'true')
  await expect(page.getByRole('tabpanel')).toBeVisible()
  await users.focus()
  await page.keyboard.press('ArrowRight')
  await expect(page).toHaveURL(/#\/servers\/1\/status$/)
  await expect(page.getByRole('tab', { name: 'Status' })).toBeFocused()
  await expect(page.getByRole('tab', { name: 'Status' })).toHaveAttribute('aria-selected', 'true')
  await page.keyboard.press('End')
  await expect(page).toHaveURL(/#\/servers\/1\/logs$/)
  await page.keyboard.press('ArrowRight')
  await expect(page).toHaveURL(/#\/servers\/1$/)
})

test('menus say they open, move with arrow keys and give focus back', async ({ page }) => {
  await openServer(page, FRA)
  const more = page.getByRole('button', { name: 'More actions' })
  await expect(more).toHaveAttribute('aria-haspopup', 'menu')
  await expect(more).toHaveAttribute('aria-expanded', 'false')
  await more.click()
  await expect(more).toHaveAttribute('aria-expanded', 'true')
  await expect(page.getByRole('menuitem', { name: /Renew link keys/ })).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(page.getByRole('menuitem', { name: 'Rename' })).toBeFocused()
  await page.keyboard.press('End')
  await expect(page.getByRole('menuitem', { name: 'Remove server…' })).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('menu')).toBeHidden()
  await expect(more).toBeFocused()
})

test('dialogs keep focus inside and give it back when they close', async ({ page }) => {
  await openServer(page, FRA)
  const rename = page.getByRole('button', { name: 'More actions' })
  await rename.click()
  await page.getByRole('menuitem', { name: 'Rename' }).click()
  const d = dialog(page, 'Rename server')
  await expect(d.getByLabel('Name')).toBeFocused()
  for (let i = 0; i < 6; i++) {
    await page.keyboard.press('Tab')
    expect(await d.evaluate((el) => el.contains(document.activeElement))).toBe(true)
  }
  await page.keyboard.press('Shift+Tab')
  expect(await d.evaluate((el) => el.contains(document.activeElement))).toBe(true)
  await page.keyboard.press('Escape')
  await expect(d).toBeHidden()
  await expect(rename).toBeFocused()
})
