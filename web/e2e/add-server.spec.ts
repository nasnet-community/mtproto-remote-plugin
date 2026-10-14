import type { Page } from '@playwright/test'
import { DOMESTIC_HINT, NEW_KEY, PAGE, dialog, expect, stateHolds, test, toast } from './fixtures'

// The sample router is at its cap; make room for one more.
test.beforeEach(async ({ request }) => {
  await request.post('/api/__reset', { data: { maxServers: 4 } })
})

const installing = (page: Page, host: string) => dialog(page, new RegExp(`Installing ${host}|Server added|Installation failed`))

async function openForm(page: Page) {
  await page.goto('/')
  await page.getByRole('button', { name: 'Add server' }).click()
  return dialog(page, 'Add a server')
}

async function fillForm(page: Page, host: string, extra: { port?: string; user?: string; cover?: string; first?: string; hub?: string } = {}) {
  const add = await openForm(page)
  await add.getByLabel('Server address').fill(host)
  if (extra.port) await add.getByLabel('SSH port').fill(extra.port)
  if (extra.user) await add.getByLabel('Username').fill(extra.user)
  await add.getByLabel('Password').fill('hunter2')
  await add.getByLabel('Cover site').fill(extra.cover ?? 'www.example.com')
  await add.getByLabel('First user').fill(extra.first ?? 'first')
  if (extra.hub) {
    await add.getByRole('button', { name: 'Advanced' }).click()
    await add.getByLabel('Link port').fill(extra.hub)
  }
  await add.getByRole('button', { name: 'Connect and install' }).click()
  return add
}

/** The bodies of install requests. */
function installs(page: Page) {
  const sent: Record<string, unknown>[] = []
  page.on('request', (r) => r.method() === 'POST' && r.url().endsWith('/api/servers') && sent.push(JSON.parse(r.postData() ?? '{}')))
  return sent
}

test('the form explains the domestic list and keeps the limit and ports under Advanced', async ({ page }) => {
  const add = await openForm(page)
  await expect(add).toContainText(DOMESTIC_HINT)
  await expect(add.getByLabel('Server address')).toHaveAccessibleDescription(DOMESTIC_HINT)
  await expect(add.getByLabel('Cover site')).toHaveValue('')
  await expect(add).toContainText("The server's SSH key is saved on first connect. You're warned if it ever changes.")
  await expect(add.getByLabel('Link port')).toHaveCount(0)
  await add.getByRole('button', { name: 'Advanced' }).click()
  // 6,000 less 2,000, 1,500 and 1,000 for the three servers
  await expect(add.getByRole('slider', { name: /Connections/ })).toHaveValue('1500')
  await expect(add).toContainText('How many Telegram connections this server can hold at once. 1,500 free on this router.')
  await expect(add.getByLabel('Link port')).toHaveValue('8443')
  await expect(add.getByLabel('Capture port')).toHaveValue('9443')
  await expect(add).toContainText("The link port must be open on the server's firewall.")
})

test('the login method is a radio group that moves with the arrow keys', async ({ page }) => {
  const add = await openForm(page)
  const password = add.getByRole('radio', { name: 'Password' })
  await expect(password).toBeChecked()
  await password.focus()
  await page.keyboard.press('ArrowRight')
  await expect(add.getByRole('radio', { name: 'Private key' })).toBeChecked()
  await expect(add.getByRole('radio', { name: 'Private key' })).toBeFocused()
  await expect(add.getByLabel('Private key')).toBeVisible()
  await page.keyboard.press('ArrowLeft')
  await expect(password).toBeChecked()
})

test('adds a new server: a first key is trusted, every step is listed from the start, then its page', async ({ page }) => {
  const sent = installs(page)
  await fillForm(page, 'new.example.com')
  const job = installing(page, 'new.example.com')
  await expect(dialog(page, 'Installing new.example.com')).toBeVisible()
  // a first connection pins the key without a question
  expect(sent[0]).toMatchObject({ hostKey: NEW_KEY, hubPort: 8443, capturePort: 9443, maxStreams: 1500 })
  await expect(job).toContainText(/Step \d+ of 11 · \d+:\d\d so far/)
  await expect(job).toContainText('Connect to the agent, not started')
  // the upload says how far along it is
  await expect(job).toContainText(/\d+% · [\d.]+ MB sent · /, { timeout: 10000 })
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  await expect(job).toContainText('Connect to the agent, done')
  await expect(job).toContainText(/Took \d+:\d\d/)
  await expect(toast(page, 'new.example.com added')).toBeVisible()
  await job.getByRole('button', { name: 'Open server' }).click()

  await expect(page.getByRole('heading', { name: 'new.example.com' })).toBeVisible()
  await expect(page.locator('main').getByText('link up', { exact: true })).toBeVisible()
  await expect(page.getByRole('row', { name: /first/ })).toBeVisible()
})

test('the install dialog can be closed: the install goes on in the job tray and still says how it ended', async ({ page }) => {
  await fillForm(page, 'tray.example.com')
  const job = dialog(page, 'Installing tray.example.com')
  await expect(job).toContainText('You can close this. The install keeps going, and the job tray at the top shows it')
  await expect(job.getByRole('button', { name: 'Close; the install keeps going' })).toBeVisible()
  await job.getByRole('button', { name: 'Close', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  const tray = page.getByRole('button', { name: 'Installing tray.example.com' })
  await expect(tray).toBeVisible()
  await tray.click()
  await expect(page.getByRole('region', { name: 'Jobs' })).toContainText(/step \d+ of 11/)
  await expect(toast(page, 'tray.example.com added')).toBeVisible({ timeout: 15000 })
  // the list picks up the new server by itself
  await expect(page.getByRole('button', { name: /tray\.example\.com.*Working/ })).toBeVisible()
  // the open tray moves it to the recent results
  await expect(page.getByRole('button', { name: 'Recent jobs' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Jobs' })).toContainText('tray.example.com added')
})

test('a step that turns out to be needed appears in its place', async ({ page }) => {
  await fillForm(page, 'nft.example.com')
  const job = installing(page, 'nft.example.com')
  await expect(job).toContainText('Check the server')
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  const steps = (await job.getByRole('listitem').allTextContents()).map((t) => t.split(',')[0])
  expect(steps.slice(2, 5)).toEqual(['Check the server', 'Install nftables', 'Check the downloads'])
})

test('an address that is already a server is refused, pointing at repair', async ({ page }) => {
  const add = await fillForm(page, 'hel.example.net', { port: '2222', user: 'deploy' })
  await expect(add).toContainText('hel.example.net is already a server here: use repair on its page')
  await expect(add.getByLabel('Server address')).toHaveValue('hel.example.net')
})

test('logs in with a private key and never keeps it', async ({ page }) => {
  const sent = installs(page)
  const add = await openForm(page)
  await add.getByLabel('Server address').fill('key.example.com')
  await add.getByRole('radio', { name: 'Private key' }).click()
  await expect(add.getByLabel('Password')).toHaveCount(0)
  await add.getByLabel('Private key').fill('-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-key-body\n-----END OPENSSH PRIVATE KEY-----')
  await add.getByLabel('Key passphrase').fill('open sesame')
  await add.getByLabel('Cover site').fill('www.example.com')
  await add.getByLabel('First user').fill('')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })

  const body = sent[0]
  expect(body.privateKey).toContain('secret-key-body')
  expect(body.passphrase).toBe('open sesame')
  expect(body.password).toBeUndefined()
  expect(body.firstUser).toBeUndefined()
  const stored = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))
  expect(stored).not.toContain('secret-key-body')
  expect(stored).not.toContain('open sesame')
})

test('a failure at the link step names the chosen port and explains the domestic list', async ({ page }) => {
  await fillForm(page, 'nolink.example.com', { hub: '9100' })
  const failed = dialog(page, 'Installation failed')
  await expect(failed).toBeVisible({ timeout: 15000 })
  await expect(failed).toContainText('the router cannot reach it on TCP port 9100')
  await expect(failed).toContainText(DOMESTIC_HINT)
  await expect(failed).toContainText(/Failed at step 11 of 11/)
  await expect(toast(page, 'Installation failed')).toBeVisible()
  // The server is saved, so it can be repaired or removed from its page.
  await failed.getByRole('button', { name: 'Open server' }).click()
  await expect(page.getByRole('heading', { name: 'nolink.example.com' })).toBeVisible()
  await expect(page.locator('main').getByText('link down', { exact: true })).toBeVisible()
})

test('a cover site that fails its test is refused before anything is installed', async ({ page }) => {
  const add = await fillForm(page, 'cover.example.com', { cover: 'bad.example.com' })
  await expect(add.getByRole('button', { name: 'Testing the cover site…' })).toBeVisible()

  // back on the form, the login kept: only the site has to change
  await expect(add).toContainText("bad.example.com doesn't work as a cover site from this server")
  await expect(add).toContainText('bad.example.com does not offer TLS 1.3')
  await expect(add).toContainText('Resolves to 192.0.2.44')
  await expect(add).not.toContainText('ms from the server')
  await expect(add.getByLabel('Password')).toHaveValue('hunter2')
  await expect(page.getByRole('dialog', { name: /Installing|Installation failed/ })).toHaveCount(0)

  await add.getByLabel('Cover site').fill('www.example.com')
  await expect(add).not.toContainText("doesn't work as a cover site")
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(installing(page, 'cover.example.com')).toContainText('Check the cover site')
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
})

test('a wrong password is refused before anything is installed', async ({ page }) => {
  const add = await openForm(page)
  await add.getByLabel('Server address').fill('pw.example.com')
  await add.getByLabel('Password').fill('wrong')
  await add.getByLabel('Cover site').fill('www.example.com')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('the password or key was not accepted')
  await expect(add.getByLabel('Server address')).toHaveValue('pw.example.com')
})

test('warns about a changed host key and needs a confirmation', async ({ page }) => {
  const add = await fillForm(page, 'changed.example.com')
  await expect(add).toContainText('shows a different key than last time')
  const trust = add.getByRole('button', { name: 'Trust and install' })
  await expect(trust).toBeDisabled()
  await add.getByLabel('I know why the key changed').check()
  await expect(trust).toBeEnabled()
  await add.getByRole('button', { name: 'Back' }).click()
  await expect(add.getByLabel('Server address')).toHaveValue('changed.example.com')
})

test('shows a failed installation and lets you retry', async ({ page }) => {
  await fillForm(page, 'fail.example.com', { user: 'ubuntu' })
  const failed = dialog(page, 'Installation failed')
  await expect(failed).toBeVisible({ timeout: 15000 })
  await expect(failed).toContainText('this server does not run systemd')
  // the steps it never reached stay greyed
  await expect(failed).toContainText('Connect to the agent, not started')
  await failed.getByRole('button', { name: 'Try again' }).click()
  const again = dialog(page, 'Add a server')
  await expect(again.getByLabel('Server address')).toHaveValue('fail.example.com')
  await expect(again.getByLabel('Password')).toHaveValue('')
})

test('follows the install by polling when its stream is unavailable', async ({ page }) => {
  await fillForm(page, 'poll.example.com')
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  await expect(dialog(page, 'Server added')).toContainText('Connect to the agent')
})

test('reports a server it cannot reach', async ({ page }) => {
  const add = await fillForm(page, 'offline.example.com')
  await expect(add).toContainText('connection refused')
  await expect(add.getByLabel('Server address')).toBeVisible()
})

test('checks the first user, the cover site and the ports before connecting', async ({ page }) => {
  const probes: string[] = []
  page.on('request', (r) => r.method() === 'POST' && r.url().endsWith('/api/probe') && probes.push(r.url()))
  const add = await openForm(page)
  await add.getByLabel('Server address').fill('x.example.com')
  await add.getByLabel('Password').fill('pw')
  await add.getByLabel('Cover site').fill('www.example.com')
  await add.getByLabel('First user').fill('bad name')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('Name the first user with letters')
  await add.getByLabel('First user').fill('ok')
  await add.getByLabel('Cover site').fill('not a domain')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('Give the cover site as a domain name')
  await add.getByLabel('Cover site').fill('www.example.com')
  await add.getByRole('button', { name: 'Advanced' }).click()
  await add.getByLabel('Link port').fill('22')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('Ports run from 1024 to 65535 and must differ from the SSH port.')
  await add.getByLabel('Link port').fill('9443')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('The link port and the capture port must differ.')
  expect(probes).toEqual([])
})

test('the connection limit starts within what is left on the router', async ({ page, request }) => {
  // 3000 + 1500 + 1000 of 6000: 500 left
  await request.patch('/api/servers/1/settings', { data: { maxStreams: 3000 }, headers: PAGE })
  const sent = installs(page)
  const add = await openForm(page)
  await add.getByRole('button', { name: 'Advanced' }).click()
  const limit = add.getByRole('slider', { name: /Connections/ })
  await expect(limit).toHaveValue('500')
  await expect(limit).toHaveAttribute('max', '500')
  await expect(add).toContainText('500 free on this router.')
  await add.getByLabel('Server address').fill('room.example.com')
  await add.getByLabel('Password').fill('hunter2')
  await add.getByLabel('Cover site').fill('www.example.com')
  await limit.focus()
  await page.keyboard.press('ArrowLeft')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  expect(sent[0].maxStreams).toBe(400)
})

test('a router with no connections left says so before connecting', async ({ page, request }) => {
  await request.patch('/api/servers/1/settings', { data: { maxStreams: 3500 }, headers: PAGE })
  const add = await openForm(page)
  await add.getByLabel('Server address').fill('full.example.com')
  await add.getByLabel('Password').fill('hunter2')
  await add.getByLabel('Cover site').fill('www.example.com')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText("This router has no connections left; lower another server's limit first.")
})

test('closing the dialog drops the typed login, however it is closed', async ({ page }) => {
  const closes: [string, string, (p: Page) => Promise<void>][] = [
    ['Cancel', 'drop.example.com', (p) => dialog(p, 'Add a server').getByRole('button', { name: 'Cancel' }).click()],
    ['the close button', 'drop.example.com', (p) => dialog(p, 'Add a server').getByRole('button', { name: 'Close' }).click()],
    ['Escape', 'drop.example.com', (p) => p.keyboard.press('Escape')],
    ['the overlay', 'drop.example.com', (p) => p.mouse.click(5, 5)],
    [
      'Back, then Cancel',
      // only a changed key stops to ask
      'changed.example.com',
      async (p) => {
        const add = dialog(p, 'Add a server')
        await add.getByRole('button', { name: 'Connect and install' }).click()
        await add.getByRole('button', { name: 'Back' }).click()
        await add.getByRole('button', { name: 'Cancel' }).click()
      },
    ],
  ]
  for (const [how, host, close] of closes) {
    const add = await openForm(page)
    await add.getByLabel('Server address').fill(host)
    await add.getByLabel('Cover site').fill('www.example.com')
    await add.getByLabel('Password').fill('hunter2-typed')
    expect(await stateHolds(page, 'hunter2-typed'), how).toBe(true) // the check can see it
    await close(page)
    await expect(add).toBeHidden()
    expect(await stateHolds(page, 'hunter2-typed'), how).toBe(false)
  }
  // the key form too
  const add = await openForm(page)
  await add.getByRole('radio', { name: 'Private key' }).click()
  await add.getByLabel('Private key').fill('typed-key-body')
  await add.getByLabel('Key passphrase').fill('typed-phrase')
  await page.keyboard.press('Escape')
  expect(await stateHolds(page, 'typed-key-body')).toBe(false)
  expect(await stateHolds(page, 'typed-phrase')).toBe(false)
})
