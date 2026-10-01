import { type BrowserContext, expect, type Page, test } from '@playwright/test'

import {
  createLink,
  expectAccessible,
  expectNoMotion,
  focusStop,
  readout,
  screenRows,
  signIn,
  startTerminal,
  tabKey,
  tabTo,
} from './support'

// WCAG checks with axe (contrast included), keyboard-only traversal with
// visible focus at every stop, and reduced motion, on every kind of screen:
// sign-in, the admin console, a live terminal as owner and as viewer, and
// playback.
test.describe.configure({ mode: 'serial', timeout: 120_000 })

let admin: BrowserContext
let page: Page
let terminalUrl = ''
let viewerUrl = ''

test.beforeAll(async ({ browser }) => {
  admin = await browser.newContext()
  page = await admin.newPage()
  await signIn(page)
})

test.afterAll(async () => {
  await admin.close()
})

test('sign-in passes axe and works with the keyboard alone', async ({ browser }) => {
  const context = await browser.newContext()
  const p = await context.newPage()
  await p.goto('/admin')
  await expect(p.getByLabel('Administrator password')).toBeVisible()
  await expectAccessible(p, 'sign-in')

  await tabTo(p, /^input:Administrator password/)
  await p.keyboard.type('not the password')
  const stops = await tabTo(p, /^button:Sign in/)
  expect(stops.at(-1)).toBe('button:Sign in')
  await p.keyboard.press('Enter')
  const error = p.getByText('That password is not correct.')
  await expect(error).toBeVisible()
  await expectAccessible(p, 'sign-in with an error')
  await context.close()
})

test('every admin page passes axe and the navigation is keyboard-operable', async () => {
  for (const [path, heading] of [
    ['/admin', 'Overview'],
    ['/admin/sessions', 'Live sessions'],
    ['/admin/recordings', 'Recordings'],
    ['/admin/access', 'Access grants'],
    ['/admin/audit', 'Audit log'],
    ['/admin/settings', 'Settings'],
  ]) {
    await page.goto(path)
    await expect(page.getByRole('heading', { level: 1, name: heading })).toBeVisible()
    await expectAccessible(page, path)
  }

  await page.goto('/admin')
  await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible()
  await tabTo(page, /^a:Audit log/)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('heading', { level: 1, name: 'Audit log' })).toBeVisible()
  await expect(page).toHaveURL(/\/admin\/audit$/)
})

test('the new-terminal form passes axe and is reachable by keyboard', async () => {
  await page.goto('/admin/sessions')
  await expect(page.getByRole('heading', { level: 1, name: 'Live sessions' })).toBeVisible()
  await tabTo(page, /^button:New terminal/)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('form', { name: 'New terminal' })).toBeVisible()
  await expectAccessible(page, 'new terminal form')
})

test('the live owner workspace passes axe and every stop, the terminal included, shows focus', async () => {
  await startTerminal(page)
  terminalUrl = page.url()
  await page.locator('.xterm').click()
  await page.keyboard.type('echo a11y-$((2*3))\n')
  await expect(screenRows(page)).toContainText('a11y-6')
  await expectAccessible(page, 'live owner')

  await page.goto(terminalUrl)
  await expect(readout(page)).toContainText('Live')
  await tabTo(page, /^terminal$/)
  await page.keyboard.press('Escape')
  await tabTo(page, /^button:Terminate/)
  await page.keyboard.press('Enter')
  const dialog = page.getByRole('alertdialog', { name: 'Terminate this terminal?' })
  await expect(dialog).toBeVisible()
  await expectAccessible(page, 'terminate confirmation')
  for (let i = 0; i < 6; i++) {
    await page.keyboard.press(tabKey())
    expect(await dialog.evaluate((d) => d.contains(document.activeElement)), 'focus stays in the dialog').toBe(true)
    expect((await focusStop(page)).visible, 'focus is visible in the dialog').toBe(true)
  }
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await expect(readout(page)).toContainText('Live')
  viewerUrl = await createLink(page, 'Viewer', 'a11y viewer')
})

test('the live viewer passes axe and Tab passes the read-only terminal with focus visible', async ({ browser }) => {
  const context = await browser.newContext()
  const viewer = await context.newPage()
  await viewer.goto(viewerUrl)
  await expect(viewer.getByText('You joined as a viewer.')).toBeVisible()
  await expect(screenRows(viewer)).toContainText('a11y-6')
  await expectAccessible(viewer, 'live viewer')
  const stops = await tabTo(viewer, /^button:Leave/)
  expect(stops).not.toContain('terminal')
  await context.close()
})

test('playback passes axe and its transport is keyboard-complete with focus visible', async () => {
  await page.goto(terminalUrl)
  await page.getByRole('button', { name: 'Terminate', exact: true }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Terminate' }).click()
  await expect(readout(page)).toHaveText(/Terminated|Ended/)

  await page.goto('/admin/recordings')
  const table = page.getByRole('table', { name: 'Recordings' })
  await expect(async () => {
    await page.reload()
    await expect(table.getByText('Complete').first()).toBeVisible({ timeout: 1000 })
  }).toPass({ timeout: 15_000 })
  await table.getByRole('link', { name: /^Play recording/ }).first().click()
  await expect(readout(page)).toContainText('Playback')
  await expectAccessible(page, 'playback')

  await tabTo(page, /^input:Position|^Position/)
  await page.keyboard.press('End')
  await expect(screenRows(page)).toContainText('a11y-6')
  await tabTo(page, /^button:Play$/)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: 'Pause' })).toBeFocused()
})

test('nothing animates when reduced motion is requested', async ({ browser }) => {
  const context = await browser.newContext({ reducedMotion: 'reduce', storageState: await admin.storageState() })
  const p = await context.newPage()
  for (const path of ['/admin', '/admin/sessions', '/admin/recordings', '/admin/audit']) {
    await p.goto(path)
    await expect(p.getByRole('navigation', { name: 'Admin' })).toBeVisible()
    await expectNoMotion(p, path)
  }
  await p.goto('/admin/recordings')
  await p.getByRole('table', { name: 'Recordings' }).getByRole('link', { name: /^Play recording/ }).first().click()
  await expect(readout(p)).toContainText('Playback')
  await expectNoMotion(p, 'playback')
  await context.close()

  const anonymous = await browser.newContext({ reducedMotion: 'reduce' })
  const signInPage = await anonymous.newPage()
  await signInPage.goto('/admin')
  await expect(signInPage.getByLabel('Administrator password')).toBeVisible()
  await expectNoMotion(signInPage, 'sign-in')
  await anonymous.close()
})
