import type { Page } from '@playwright/test'
import { dialog, expect, test } from './fixtures'

async function fillForm(page: Page, host: string, extra: { port?: string; user?: string } = {}) {
  await page.goto('/')
  await page.getByRole('button', { name: 'Add server' }).click()
  const add = dialog(page, 'Add a server')
  await add.getByLabel('Server address').fill(host)
  if (extra.port) await add.getByLabel('SSH port').fill(extra.port)
  if (extra.user) await add.getByLabel('Username').fill(extra.user)
  await add.getByLabel('Password').fill('hunter2')
  await add.getByLabel('Disguise as').fill('www.google.com')
  await add.getByLabel('First user').fill('first')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  return add
}

test('adds a new server: host key, progress, then its page', async ({ page }) => {
  const add = await fillForm(page, 'new.example.com')
  await expect(add).toContainText('First connection to new.example.com')
  await expect(add).toContainText('SHA256:u3Rq0Q1uV3Jc0mYw7b6o8xk3j2m9Pq5c1f0aZyXwVb8')
  await add.getByRole('button', { name: 'Trust and install' }).click()

  const installing = dialog(page, /Installing the proxy|Server added/)
  await expect(installing).toContainText('Connect to new.example.com:22')
  await expect(installing).toContainText('Download the proxy')
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
  await expect(installing).toContainText('Key login works; the password is not kept')
  await expect(installing).toContainText('The proxy (3.5.9) is running with 1 user')
  await installing.getByRole('button', { name: 'Open server' }).click()

  await expect(page.getByRole('heading', { name: 'new.example.com' })).toBeVisible()
  await expect(page.getByRole('row', { name: /first/ })).toBeVisible()
})

test('skips the host key question for a known server', async ({ page }) => {
  await fillForm(page, 'hel.example.net', { port: '2222', user: 'deploy' })
  await expect(dialog(page, /Installing the proxy|Server added/)).toContainText('Logged in as deploy')
  await expect(dialog(page, 'Server added')).toBeVisible({ timeout: 15000 })
})

test('keeps an existing configuration and its users', async ({ page }) => {
  const add = await fillForm(page, 'kept.example.com')
  await add.getByRole('button', { name: 'Trust and install' }).click()
  const done = dialog(page, 'Server added')
  await expect(done).toBeVisible({ timeout: 15000 })
  await expect(done).toContainText('Kept the existing configuration and its users')
  await expect(done).toContainText('running with 3 users (existing configuration kept)')
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
  await expect(failed).toContainText('ubuntu is not in the sudoers file')
  await failed.getByRole('button', { name: 'Try again' }).click()
  await expect(dialog(page, 'Add a server').getByLabel('Server address')).toBeVisible()
})

test('reports a server it cannot reach', async ({ page }) => {
  const add = await fillForm(page, 'offline.example.com')
  await expect(add).toContainText('Connection refused')
  await expect(add.getByLabel('Server address')).toBeVisible()
})

test('checks the first user name before connecting', async ({ page }) => {
  await page.goto('/')
  await page.getByRole('button', { name: 'Add server' }).click()
  const add = dialog(page, 'Add a server')
  await add.getByLabel('Server address').fill('x.example.com')
  await add.getByLabel('Password').fill('pw')
  await add.getByLabel('Disguise as').fill('www.google.com')
  await add.getByLabel('First user').fill('bad name')
  await add.getByRole('button', { name: 'Connect and install' }).click()
  await expect(add).toContainText('Name the first user with letters')
})
