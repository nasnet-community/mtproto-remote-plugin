import { FRA, HEL, alertDialog, dialog, expect, openServer, test, toast } from './fixtures'

const card = (page: import('@playwright/test').Page, name: string) => page.getByRole('listitem').filter({ hasText: new RegExp(`^${name}`) })

test('on a phone, users are cards with a Share button, and every action is reachable', async ({ page }) => {
  await openServer(page, FRA)
  await expect(page.getByRole('table')).toBeHidden()
  const alice = card(page, 'alice')
  await expect(alice).toContainText("Mom's tablet")
  await expect(alice).toContainText('2 of 3 devices')
  await expect(alice).toContainText('3.9 of 50 GB · 8%')
  await expect(alice).toContainText('Expires')
  await alice.getByRole('button', { name: 'Share' }).click()
  await expect(dialog(page, "Share alice's proxy").getByRole('img', { name: 'QR code of the proxy link' })).toBeVisible()
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'Actions for alice' }).click()
  await expect(page.getByRole('menuitem', { name: "Replace this user's link" })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(card(page, 'trial-7d').getByRole('button', { name: 'Renew +30 days' })).toBeVisible()

  await page.getByRole('switch', { name: 'Pause alice' }).click()
  const done = toast(page, 'alice paused')
  await expect(done).toBeVisible()
  // phones: toasts sit at the bottom, clear of the header
  const box = (await done.boundingBox())!
  const vh = page.viewportSize()!.height
  expect(box.y).toBeGreaterThan(vh / 2)
  expect(box.y + box.height).toBeLessThanOrEqual(vh - 12)
})

test('on a phone, Select mode acts on several users from a bar at the bottom', async ({ page }) => {
  await openServer(page, FRA)
  await page.getByRole('button', { name: 'Select' }).click()
  await expect(page.getByText('0 selected').filter({ visible: true })).toBeVisible()
  await expect(page.getByText('Frankfurt VPS · tap users to add or remove them')).toBeVisible()
  await page.getByRole('checkbox', { name: /^bob/ }).check()
  await page.getByRole('checkbox', { name: /^guest-wifi/ }).check()
  await expect(page.getByText('2 selected').filter({ visible: true })).toBeVisible()
  const resume = page.getByRole('button', { name: 'Resume' })
  const bar = (await resume.boundingBox())!
  expect(bar.y + bar.height).toBeGreaterThan(page.viewportSize()!.height - 80)
  await resume.click()
  await expect(toast(page, '2 users resumed')).toBeVisible()
  // done, so back to the cards
  await expect(page.getByRole('button', { name: 'Select' })).toBeVisible()
  await expect(card(page, 'bob')).toContainText('Active')

  await page.getByRole('button', { name: 'Select' }).click()
  await page.getByRole('button', { name: 'Select all 22' }).click()
  await expect(page.getByText('22 selected').filter({ visible: true })).toBeVisible()
  await page.getByRole('button', { name: 'Leave select mode' }).click()
  await expect(page.getByText('22 selected').filter({ visible: true })).toHaveCount(0)
})

test('on a phone, the tiles sit in one row and no page scrolls sideways', async ({ page }) => {
  await openServer(page, FRA)
  const tiles = page.locator('main .grid-cols-4 > div')
  await expect(tiles).toHaveCount(4)
  const tops = await tiles.evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().top)))
  expect(new Set(tops).size).toBe(1)
  await expect(tiles.first()).toContainText('17/22', { useInnerText: true })

  for (const path of ['/', '/#/servers/1', '/#/servers/1/status', '/#/servers/2/status', '/#/servers/1/settings', '/#/servers/2/logs', '/#/servers/3']) {
    await page.goto(path)
    await page.waitForLoadState('networkidle')
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
    expect(overflow, path).toBeLessThanOrEqual(0)
  }
})

test('on a phone, the controls are big enough to tap', async ({ page }) => {
  await openServer(page, FRA)
  await expect(card(page, 'alice')).toBeVisible()
  const controls = [
    ...(await page.getByRole('group', { name: 'Server actions' }).getByRole('button').all()),
    ...(await page.getByRole('tab').all()),
    page.getByRole('button', { name: 'All servers' }),
    page.getByRole('button', { name: /^All \d+$/ }),
    page.getByRole('button', { name: 'Add', exact: true }),
    page.getByRole('button', { name: 'Select' }),
    page.getByRole('button', { name: /^Sort: / }),
    page.getByRole('switch', { name: 'Pause alice' }),
    card(page, 'alice').getByRole('button', { name: 'Share' }),
    page.getByRole('button', { name: 'Actions for alice' }),
    page.getByRole('button', { name: 'Toggle light or dark theme' }),
  ]
  for (const c of controls) {
    const box = (await c.boundingBox())!
    expect(box.height, (await c.getAttribute('aria-label')) ?? (await c.textContent()) ?? '').toBeGreaterThanOrEqual(44)
  }
})

test('on a phone, link connections are stacked rows', async ({ page }) => {
  await openServer(page, HEL, 'status')
  await expect(page.getByRole('table')).toBeHidden()
  const rows = page.getByRole('listitem').filter({ hasText: /^tcp-/ })
  await expect(rows.first()).toContainText(/Since .+ · round trip \d+ ms · \d+ connections/)
  await expect(rows.nth(1)).toContainText('dial tcp hel.example.net:8443: i/o timeout')
})

test('on a phone, a running job shows as a small chip with a count', async ({ page }) => {
  await openServer(page, HEL)
  await page.getByRole('button', { name: 'Update to 0.5.0' }).click()
  await alertDialog(page, /Update the agent/).getByRole('button', { name: 'Update agent' }).click()
  await dialog(page, /Updating the agent/).getByRole('button', { name: 'Close', exact: true }).click()
  const chip = page.getByRole('button', { name: 'Updating the agent · Helsinki ARM' })
  await expect(chip).toHaveText('1', { useInnerText: true })
  await chip.click()
  await expect(page.getByRole('region', { name: 'Jobs' })).toBeVisible()
  await expect(toast(page, 'Agent updated')).toBeVisible({ timeout: 10000 })
})

test('on a phone, a toast moves up while a sheet is open, clear of its buttons', async ({ page }) => {
  await openServer(page, FRA)
  await page.getByRole('button', { name: 'Add', exact: true }).click()
  const add = dialog(page, 'Add a user')
  await add.getByLabel('Name').fill('zoe')
  await add.getByRole('button', { name: 'Add user' }).click()
  const share = dialog(page, "Share zoe's proxy")
  const done = toast(page, 'zoe added')
  await expect(done).toBeVisible()
  const t = (await done.boundingBox())!
  const sheet = (await share.boundingBox())!
  expect(t.y + t.height).toBeLessThanOrEqual(sheet.y)
})
