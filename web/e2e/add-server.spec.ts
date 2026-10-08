import type { Page } from '@playwright/test'
import { DOMESTIC_HINT, PAGE, dialog, expect, stateHolds, test, toast } from './fixtures'

// The sample router is at its cap; make room for one more.
test.beforeEach(async ({ request }) => {
  await request.post('/api/__reset', { data: { maxServers: 4 } })
})

async function openForm(page: Page) {
  await page.goto('/')
  await page.getByRole('button', { name: 'Add server' }).click()
  return dialog(page, 'Add a server')
}

async function fillForm(
  page: Page,
  host: string,
  extra: { port?: string; user?: string; cover?: string; first?: string } = {},
) {
  const add = await openForm(page)
  await add.getByLabel('Server address').fill(host)
  if (extra.port) await add.getByLabel('SSH port').fill(extra.port)
  if (extra.user) await add.getByLabel('Username').fill(extra.user)
  await add.getByLabel('Password').fill('hunter2')
  await add.getByLabel('Cover site').fill(extra.cover ?? 'www.example.com')
  await add.getByLabel('First user').fill(extra.first ?? 'first')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  return add
}

test('the form explains the domestic address list', async ({ page }) => {
  const add = await openForm(page)
  await expect(add).toContainText(DOMESTIC_HINT)
  await expect(add.getByLabel('Cover site')).toHaveValue('')
})

test('adds a new server: host key, steps, then its page', async ({ page }) => {
  const add = await fillForm(page, 'new.example.com')
  await expect(add).toContainText('First connection to new.example.com')
  await expect(add).toContainText('SHA256:u3Rq0Q1uV3Jc0mYw7b6o8xk3j2m9Pq5c1f0aZyXwVb8')
  await add.getByRole('button', { name: 'Trust and install' }).click()

  const installing = dialog(page, /Installing|Server added/)
  await expect(installing).toContainText('Check the server')
  await expect(installing).toContainText('Check the cover site')
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  await expect(installing).toContainText('Connect to the agent')
  await expect(toast(page, 'new.example.com added')).toBeVisible()
  await installing.getByRole('button', { name: 'Open server' }).click()

  await expect(page.getByRole('heading', { name: 'new.example.com' })).toBeVisible()
  await expect(page.locator('main').getByText('link up', { exact: true })).toBeVisible()
  await expect(page.getByRole('row', { name: /first/ })).toBeVisible()
})

test('an address that is already a server is refused, pointing at repair', async ({ page }) => {
  // its host key is known, so there is no key question first
  const add = await fillForm(page, 'hel.example.net', { port: '2222', user: 'deploy' })
  await expect(add).toContainText('hel.example.net is already a server here: use repair on its page')
  await expect(add).not.toContainText('First connection')
  await expect(add.getByLabel('Server address')).toHaveValue('hel.example.net')
})

test('logs in with a private key and never keeps it', async ({ page }) => {
  const sent: string[] = []
  page.on('request', (r) => r.method() === 'POST' && r.url().endsWith('/api/servers') && sent.push(r.postData() ?? ''))
  const add = await openForm(page)
  await add.getByLabel('Server address').fill('key.example.com')
  await add.getByRole('radio', { name: 'Private key' }).click()
  await expect(add.getByLabel('Password')).toHaveCount(0)
  await add.getByLabel('Private key').fill('-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-key-body\n-----END OPENSSH PRIVATE KEY-----')
  await add.getByLabel('Key passphrase').fill('open sesame')
  await add.getByLabel('Cover site').fill('www.example.com')
  await add.getByLabel('First user').fill('')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await add.getByRole('button', { name: 'Trust and install' }).click()
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })

  const body = JSON.parse(sent[0])
  expect(body.privateKey).toContain('secret-key-body')
  expect(body.passphrase).toBe('open sesame')
  expect(body.password).toBeUndefined()
  expect(body.firstUser).toBeUndefined()
  const stored = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))
  expect(stored).not.toContain('secret-key-body')
  expect(stored).not.toContain('open sesame')
})

test('a failure at the link step explains the domestic address list', async ({ page }) => {
  const add = await fillForm(page, 'nolink.example.com')
  await add.getByRole('button', { name: 'Trust and install' }).click()
  const failed = dialog(page, 'Installation failed')
  await expect(failed).toBeVisible({ timeout: 15000 })
  await expect(failed).toContainText('the router cannot reach it on TCP port 8443')
  await expect(failed).toContainText(DOMESTIC_HINT)
  await expect(toast(page, 'Installation failed')).toBeVisible()
  // The server is saved, so it can be repaired or removed from its page.
  await failed.getByRole('button', { name: 'Open server' }).click()
  await expect(page.getByRole('heading', { name: 'nolink.example.com' })).toBeVisible()
  await expect(page.locator('main').getByText('link down', { exact: true })).toBeVisible()
})

test('a cover site that fails its test is refused before anything is installed', async ({ page }) => {
  const add = await fillForm(page, 'cover.example.com', { cover: 'bad.example.com' })
  await add.getByRole('button', { name: 'Trust and install' }).click()
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
  await add.getByRole('button', { name: 'Trust and install' }).click()
  const installing = dialog(page, /Installing|Server added/)
  await expect(installing).toContainText('Check the cover site')
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
})

test('a wrong password is refused before anything is installed', async ({ page }) => {
  const add = await openForm(page)
  await add.getByLabel('Server address').fill('pw.example.com')
  await add.getByLabel('Password').fill('wrong')
  await add.getByLabel('Cover site').fill('www.example.com')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await add.getByRole('button', { name: 'Trust and install' }).click()
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
  const add = await fillForm(page, 'fail.example.com', { user: 'ubuntu' })
  await add.getByRole('button', { name: 'Trust and install' }).click()
  const failed = dialog(page, 'Installation failed')
  await expect(failed).toBeVisible({ timeout: 15000 })
  await expect(failed).toContainText('this server does not run systemd')
  await failed.getByRole('button', { name: 'Try again' }).click()
  const again = dialog(page, 'Add a server')
  await expect(again.getByLabel('Server address')).toHaveValue('fail.example.com')
  await expect(again.getByLabel('Password')).toHaveValue('')
})

test('follows the install by polling when its stream is unavailable', async ({ page }) => {
  const add = await fillForm(page, 'poll.example.com')
  await add.getByRole('button', { name: 'Trust and install' }).click()
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  await expect(dialog(page, 'Server added')).toContainText('Connect to the agent')
})

test('reports a server it cannot reach', async ({ page }) => {
  const add = await fillForm(page, 'offline.example.com')
  await expect(add).toContainText('connection refused')
  await expect(add.getByLabel('Server address')).toBeVisible()
})

test('checks the first user name and the cover site before connecting', async ({ page }) => {
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
})

test('the stream limit starts within what is left and is checked before connecting', async ({ page, request }) => {
  // 3500 + 1000 + 1000 of 6000: 500 left
  await request.patch('/api/servers/1/settings', { data: { maxStreams: 3500 }, headers: PAGE })
  const probes: string[] = []
  const installs: string[] = []
  page.on('request', (r) => {
    if (r.method() !== 'POST') return
    if (r.url().endsWith('/api/probe')) probes.push(r.url())
    if (r.url().endsWith('/api/servers')) installs.push(r.postData() ?? '')
  })
  const add = await openForm(page)
  const limit = add.getByLabel('Stream limit')
  await expect(limit).toHaveValue('500')
  await add.getByLabel('Server address').fill('room.example.com')
  await add.getByLabel('Password').fill('hunter2')
  await add.getByLabel('Cover site').fill('www.example.com')
  await limit.fill('600')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('Only 500 streams are left on this router')
  await limit.fill('50')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('at least 100')
  expect(probes).toEqual([])

  await limit.fill('400')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await add.getByRole('button', { name: 'Trust and install' }).click()
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  expect(JSON.parse(installs[0]).maxStreams).toBe(400)
})

test('closing the dialog drops the typed login, however it is closed', async ({ page }) => {
  const closes: [string, (p: Page) => Promise<void>][] = [
    ['Cancel', (p) => dialog(p, 'Add a server').getByRole('button', { name: 'Cancel' }).click()],
    ['the close button', (p) => dialog(p, 'Add a server').getByRole('button', { name: 'Close' }).click()],
    ['Escape', (p) => p.keyboard.press('Escape')],
    ['the overlay', (p) => p.mouse.click(5, 5)],
    [
      'Back, then Cancel',
      async (p) => {
        const add = dialog(p, 'Add a server')
        await add.getByRole('button', { name: 'Connect and install' }).click()
        await add.getByRole('button', { name: 'Back' }).click()
        await add.getByRole('button', { name: 'Cancel' }).click()
      },
    ],
  ]
  for (const [how, close] of closes) {
    const add = await openForm(page)
    await add.getByLabel('Server address').fill('drop.example.com')
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
