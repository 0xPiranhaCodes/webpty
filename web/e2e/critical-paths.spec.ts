import {
  type Browser,
  type BrowserContext,
  devices,
  expect,
  type Page,
  test,
} from '@playwright/test'

// One server, one administrator, run in order: each step builds on the
// state the previous one left behind, the way an operator would use it.
test.describe.configure({ mode: 'serial' })

const password = 'e2e correct horse battery'
const shots = './e2e/.artifacts'
const { viewport, userAgent, deviceScaleFactor, isMobile, hasTouch } =
  devices['iPhone 13']
const phone = { viewport, userAgent, deviceScaleFactor, isMobile, hasTouch }

let admin: BrowserContext
let page: Page
const consoleProblems: string[] = []

// Asking "am I signed in?" answers 401 when the answer is no; Chrome logs
// that resource status itself. Anything else is a real problem.
const expectedStatusProbe = /\/api\/v1\/(admin|access)\/session$/

function watchConsole(p: Page, who: string) {
  p.on('console', (message) => {
    if (message.type() !== 'error' && message.type() !== 'warning') return
    if (
      /status of 401/.test(message.text()) &&
      expectedStatusProbe.test(message.location().url)
    )
      return
    consoleProblems.push(
      `${who}: ${message.text()} (${message.location().url})`,
    )
  })
  p.on('pageerror', (error) => consoleProblems.push(`${who}: ${error.message}`))
}

async function guest(browser: Browser, url: string, options = {}) {
  const context = await browser.newContext(options)
  const p = await context.newPage()
  watchConsole(p, 'guest')
  await p.goto(url)
  return { context, page: p }
}

const screen = (p: Page) => p.locator('.xterm-rows')
const readout = (p: Page) => p.getByTestId('readout')

async function createLink(
  role: 'Viewer' | 'Editor',
  label: string,
  confirmReplace = false,
) {
  const rail = page.getByRole('complementary', { name: 'Terminal details' })
  await rail.getByLabel(role, { exact: true }).check()
  await rail.getByLabel('Label').fill(label)
  await rail.getByRole('button', { name: 'Create link' }).click()
  if (confirmReplace)
    await page
      .getByRole('alertdialog')
      .getByRole('button', { name: 'Replace editor' })
      .click()
  const url = await rail.getByLabel(`${role} link`).inputValue()
  await rail.getByRole('button', { name: 'Done' }).click()
  return url
}

test.beforeAll(async ({ browser }) => {
  admin = await browser.newContext()
  page = await admin.newPage()
  watchConsole(page, 'admin')
})

test.afterAll(async () => {
  await admin.close()
})

test('first run: the default password must be replaced before anything else', async () => {
  await page.goto('/admin')
  await expect(page.getByLabel('Administrator password')).toBeVisible()
  await page.screenshot({ path: `${shots}/desktop-login.png` })

  await page.getByLabel('Administrator password').fill('CHANGEME')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(
    page.getByRole('heading', { name: 'Set the administrator password' }),
  ).toBeVisible()
  await expect(page.getByRole('navigation', { name: 'Admin' })).toHaveCount(0)

  await page.getByLabel('New password', { exact: true }).fill(password)
  await page.getByLabel('Confirm new password').fill(password)
  await page.getByRole('button', { name: 'Save password' }).click()
  await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()

  await page
    .getByRole('navigation', { name: 'Admin' })
    .getByRole('link', { name: 'Settings' })
    .click()
  await expect(page.getByText('Restart required').first()).toBeVisible()
  await expect(page.getByText(password)).toHaveCount(0)
})

test('create, open, and use a terminal', async () => {
  await page
    .getByRole('navigation', { name: 'Admin' })
    .getByRole('link', { name: 'Live sessions' })
    .click()
  await page.getByRole('button', { name: 'New terminal' }).click()
  await page.getByRole('button', { name: 'Start terminal' }).click()

  await expect(readout(page)).toContainText('Live')
  await expect(readout(page)).toContainText('Recording')
  await page.locator('.xterm').click()
  await page.keyboard.type('echo webpty-e2e-$((6*7))\n')
  await expect(screen(page)).toContainText('webpty-e2e-42')
})

let viewerUrl = ''

test('a viewer link shows the terminal read-only and never leaves the token in the URL', async ({
  browser,
}) => {
  viewerUrl = await createLink('Viewer', 'Viewer from e2e')
  expect(viewerUrl).toMatch(/\/join#token=[A-Za-z0-9_-]+$/)

  const viewer = await guest(browser, viewerUrl)
  await expect(viewer.page.getByText('You joined as a viewer.')).toBeVisible()
  expect(viewer.page.url()).not.toContain('token')
  await expect(screen(viewer.page)).toContainText('webpty-e2e-42')
  await expect(
    viewer.page.getByText(
      'You are viewing. Only the owner and the editor can type.',
    ),
  ).toBeVisible()
  await viewer.page.locator('.xterm').click()
  await viewer.page.keyboard.type('echo viewer-typed\n')
  await page.waitForTimeout(500)
  await expect(screen(page)).not.toContainText('viewer-typed')
  await expect(
    page
      .getByRole('list', { name: 'People in this terminal' })
      .getByText('Viewer'),
  ).toBeVisible()

  // Live revocation disconnects the viewer at once.
  const links = page.getByRole('list', { name: 'Links for this terminal' })
  await links.getByRole('button', { name: 'Revoke Viewer from e2e' }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: 'Revoke' })
    .click()
  await expect(readout(viewer.page)).toHaveText('Access revoked')
  await viewer.context.close()
})

test('an editor can type, and a new editor link replaces the old editor after confirmation', async ({
  browser,
}) => {
  const firstUrl = await createLink('Editor', 'First editor')
  const first = await guest(browser, firstUrl)
  await expect(first.page.getByText('You joined as an editor.')).toBeVisible()
  await expect(readout(first.page)).toHaveText('Live')
  await first.page.locator('.xterm').click()
  await first.page.keyboard.type('echo from-editor\n')
  await expect(screen(page)).toContainText('from-editor')

  await createLink('Editor', 'Second editor', true)
  await expect(readout(first.page)).toContainText('Access replaced')
  await first.context.close()
})

test('phones can watch as a viewer with typing explained as unavailable', async ({
  browser,
}) => {
  const url = await createLink('Viewer', 'Phone viewer')
  const mobile = await guest(browser, url, phone)
  await expect(mobile.page.getByText('You joined as a viewer.')).toBeVisible()
  await expect(screen(mobile.page)).toContainText('webpty-e2e-42')
  await expect(
    mobile.page.getByText(
      'You are viewing. Only the owner and the editor can type.',
    ),
  ).toBeVisible()
  await mobile.page.screenshot({
    path: `${shots}/mobile-live-terminal-viewer.png`,
  })
  await mobile.context.close()
})

test('the owner workspace and overview on desktop and phone', async ({
  browser,
}) => {
  await page.keyboard.type('ls /\n')
  await page.waitForTimeout(300)
  await page.screenshot({ path: `${shots}/desktop-live-terminal.png` })

  const state = await admin.storageState()
  const mobile = await browser.newContext({ ...phone, storageState: state })
  const m = await mobile.newPage()
  watchConsole(m, 'admin-phone')
  await m.goto(page.url())
  await expect(readout(m)).toContainText('Live')
  await expect(
    m.getByText(/Typing is off on phone-sized screens/),
  ).toBeVisible()
  await m.screenshot({ path: `${shots}/mobile-live-terminal-owner.png` })

  await m.goto('/admin')
  await expect(m.getByRole('heading', { name: 'Overview' })).toBeVisible()
  await expect(m.getByText(/1 terminal running/)).toBeVisible()
  await m.screenshot({ path: `${shots}/mobile-overview.png`, fullPage: true })
  await mobile.close()

  const loginPhone = await browser.newContext(phone)
  const lp = await loginPhone.newPage()
  await lp.goto('/admin')
  await expect(lp.getByLabel('Administrator password')).toBeVisible()
  await lp.screenshot({ path: `${shots}/mobile-login.png` })
  await loginPhone.close()
})

test('terminating ends the terminal for everyone', async () => {
  await page.getByRole('button', { name: 'Terminate', exact: true }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: 'Terminate' })
    .click()
  await expect(readout(page)).toHaveText(/Terminated|Ended/)

  await page.goto('/admin')
  await expect(page.getByText(/0 terminals running, 1 ended/)).toBeVisible()
  await page.screenshot({
    path: `${shots}/desktop-overview.png`,
    fullPage: true,
  })
})

test('the recording plays back deterministically, exports, and deletes', async ({
  browser,
}) => {
  await page
    .getByRole('navigation', { name: 'Admin' })
    .getByRole('link', { name: 'Recordings' })
    .click()
  const table = page.getByRole('table', { name: 'Recordings' })
  await expect(async () => {
    await page.reload()
    await expect(table.getByText('Complete')).toBeVisible({ timeout: 1000 })
  }).toPass({ timeout: 15_000 })

  await table
    .getByRole('link', { name: /^Play recording/ })
    .first()
    .click()
  await expect(readout(page)).toContainText('Playback')
  await expect(page.locator('.spine')).toHaveAttribute('data-state', 'playback')
  const position = page.getByRole('slider', { name: 'Position' })
  await position.focus()
  await page.keyboard.press('End')
  await expect(screen(page)).toContainText('webpty-e2e-42')
  await expect(screen(page)).toContainText('from-editor')
  await page.keyboard.press('Home')
  await expect(screen(page)).not.toContainText('webpty-e2e-42')
  await page
    .getByRole('radiogroup', { name: 'Playback speed' })
    .getByLabel('2×')
    .check()
  await page.getByRole('button', { name: 'Play' }).click()
  await expect(screen(page)).toContainText('webpty-e2e-42', { timeout: 20_000 })
  await page.getByRole('button', { name: 'Pause' }).click()
  await page.screenshot({ path: `${shots}/desktop-playback.png` })

  const exportHref = await page
    .getByRole('link', { name: 'Export as asciicast' })
    .getAttribute('href')
  const exported = await page.request.get(exportHref!)
  expect(exported.status()).toBe(200)
  const header = JSON.parse((await exported.text()).split('\n')[0])
  expect(header.version).toBe(2)

  const mobile = await browser.newContext({
    ...phone,
    storageState: await admin.storageState(),
  })
  const m = await mobile.newPage()
  watchConsole(m, 'admin-phone')
  await m.goto(page.url())
  await expect(readout(m)).toContainText('Playback')
  await m.getByRole('slider', { name: 'Position' }).focus()
  await m.keyboard.press('End')
  await m.screenshot({ path: `${shots}/mobile-playback.png` })
  await mobile.close()

  await page.getByRole('button', { name: 'Delete recording' }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: 'Delete' })
    .click()
  await expect(page.getByText('Recording deleted.')).toBeVisible()
  await expect(page.getByRole('link', { name: /^Play recording/ })).toHaveCount(
    0,
  )
})

test('audit log records the session and signing out returns to sign-in', async () => {
  await page
    .getByRole('navigation', { name: 'Admin' })
    .getByRole('link', { name: 'Audit log' })
    .click()
  const table = page.getByRole('table', { name: 'Audit events' })
  await expect(table.getByText('access.grant.revoked').first()).toBeVisible()
  await expect(table).not.toContainText(password)

  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page.getByLabel('Administrator password')).toBeVisible()
})

test('no console errors or warnings were logged', () => {
  expect(consoleProblems).toEqual([])
})
