import { FRA, dialog, expect, openServer, test, toast } from './fixtures'

test('on a phone, users are cards and every action is reachable', async ({ page }) => {
  await openServer(page, FRA)
  await expect(page.getByRole('table')).toBeHidden()
  const alice = page.getByRole('listitem').filter({ hasText: /^alice/ })
  await expect(alice).toContainText('quota 50.0 GB')
  await page.getByRole('button', { name: 'Actions for alice' }).click()
  await page.getByRole('menuitem', { name: 'Share link' }).click()
  await expect(
    dialog(page, "Share alice's proxy").getByRole('img', { name: 'QR code of the proxy link' }),
  ).toBeVisible()
  await page.keyboard.press('Escape')

  await page.getByRole('switch', { name: 'Disable alice' }).click()
  await expect(toast(page, 'alice disabled')).toBeVisible()
})

test('on a phone, the page does not scroll sideways', async ({ page }) => {
  for (const path of ['/', '/#/servers/1', '/#/servers/1/status', '/#/servers/2/status', '/#/servers/1/settings', '/#/servers/1/logs']) {
    await page.goto(path)
    await page.waitForLoadState('networkidle')
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    )
    expect(overflow, path).toBeLessThanOrEqual(0)
  }
})
