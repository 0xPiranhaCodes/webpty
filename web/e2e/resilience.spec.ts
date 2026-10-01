import { type BrowserContext, expect, type Page, test } from '@playwright/test'

import { collectConsoleProblems, createLink, damageRecording, readout, screenRows, signIn, startTerminal } from './support'

// What happens when things go wrong: destructive actions that are cancelled
// or confirmed, the network dropping mid-session, stored recordings that
// fail validation, and repeated wrong passwords.
test.describe.configure({ mode: 'serial' })

let admin: BrowserContext
let page: Page
let terminalUrl = ''
const consoleProblems: string[] = []

test.beforeAll(async ({ browser }) => {
  admin = await browser.newContext()
  page = await admin.newPage()
  collectConsoleProblems(page, consoleProblems, 'admin')
  await signIn(page)
})

test.afterAll(async () => {
  await admin.close()
})

for (const [label, viewport] of [
  ['tablet', { width: 960, height: 700 }],
  ['narrow', { width: 800, height: 900 }],
] as const) {
  test(`at ${label} width the terminal settles on one size that fits the window`, async ({ browser }) => {
    const context = await browser.newContext({ viewport, storageState: await admin.storageState() })
    const p = await context.newPage()
    const resizes: string[] = []
    p.on('websocket', (ws) => ws.on('framesent', (f) => String(f.payload).includes('"resize"') && resizes.push(String(f.payload))))
    await startTerminal(p)
    await p.waitForTimeout(1500)
    const settledCount = resizes.length
    await p.waitForTimeout(1500)
    expect(resizes.slice(settledCount), 'no resizes once the layout is stable').toEqual([])
    const rows = Number(await p.locator('.workspace__screen').getAttribute('data-rows'))
    const cellHeight = await p.locator('.xterm-rows > div').first().evaluate((el) => el.getBoundingClientRect().height)
    expect(rows * cellHeight, 'the terminal is no taller than the window').toBeLessThanOrEqual(viewport.height)
    await p.getByRole('button', { name: 'Terminate', exact: true }).click()
    await p.getByRole('alertdialog').getByRole('button', { name: 'Terminate' }).click()
    await expect(readout(p)).toHaveText(/Terminated|Ended/)
    await context.close()
  })
}

test('a dropped connection shows reconnecting, then resumes without losing or repeating output', async () => {
  let offline = false
  const sockets: { close(options?: { code?: number; reason?: string }): Promise<void> }[] = []
  await page.routeWebSocket(/\/api\/v1\/terminals\/[^/]+\/ws/, (ws) => {
    if (offline) {
      ws.close({ code: 4000, reason: 'offline' })
      return
    }
    ws.connectToServer()
    sockets.push(ws)
  })

  await startTerminal(page)
  terminalUrl = page.url()
  await page.locator('.xterm').click()
  await page.keyboard.type('echo before-$((1+1))\n')
  await expect(screenRows(page)).toContainText('before-2')

  offline = true
  await admin.setOffline(true)
  await Promise.all(sockets.splice(0).map((ws) => ws.close({ code: 4000, reason: 'offline' })))
  await expect(readout(page)).toContainText('Reconnecting')
  await expect(page.locator('.workspace__screen')).toHaveAttribute('data-input', 'off')

  offline = false
  await admin.setOffline(false)
  await expect(readout(page)).toContainText('Live', { timeout: 20_000 })
  await page.locator('.xterm').click()
  await page.keyboard.type('echo after-$((2+2))\n')
  await expect(screenRows(page)).toContainText('after-4')
  const text = await screenRows(page).innerText()
  expect(text.match(/^before-2$/gm) ?? [], 'output before the drop appears once').toHaveLength(1)
  await page.unrouteAll({ behavior: 'ignoreErrors' })
})

test('revoking a link and terminating a terminal both wait for confirmation', async () => {
  await page.goto(terminalUrl)
  await expect(readout(page)).toContainText('Live')
  await createLink(page, 'Viewer', 'Kept viewer')
  const links = page.getByRole('list', { name: 'Links for this terminal' })
  await links.getByRole('button', { name: 'Revoke Kept viewer' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByRole('alertdialog')).toBeHidden()
  await expect(links.getByText('Kept viewer')).toBeVisible()

  await page.getByRole('button', { name: 'Terminate', exact: true }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Cancel' }).click()
  await expect(readout(page)).toContainText('Live')
  await page.getByRole('button', { name: 'Terminate', exact: true }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Terminate' }).click()
  await expect(readout(page)).toHaveText(/Terminated|Ended/)
})

test('running retention needs confirmation and keeps recordings that have not expired', async () => {
  await page.goto('/admin/recordings')
  const table = page.getByRole('table', { name: 'Recordings' })
  await expect(async () => {
    await page.reload()
    await expect(table.getByText('Complete').first()).toBeVisible({ timeout: 1000 })
  }).toPass({ timeout: 15_000 })
  const before = await table.getByRole('row').count()

  await page.getByRole('button', { name: 'Delete expired recordings' }).click()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('alertdialog')).toBeHidden()
  await expect(page.getByRole('status').filter({ hasText: /expired/ })).toHaveCount(0)

  await page.getByRole('button', { name: 'Delete expired recordings' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Delete expired' }).click()
  await expect(page.getByText('No recordings had expired.')).toBeVisible()
  await expect(table.getByRole('row')).toHaveCount(before)
})

test('a recording whose stored data is damaged is refused, explained, and marked incomplete', async ({ baseURL }) => {
  const table = page.getByRole('table', { name: 'Recordings' })
  const link = table.getByRole('link', { name: /^Play recording/ }).first()
  const recordingId = decodeURIComponent((await link.getAttribute('href'))!.split('/').pop()!)
  damageRecording(baseURL!, recordingId)

  await link.click()
  await expect(page.getByRole('heading', { name: 'This recording failed validation.' })).toBeVisible()
  await page.goto('/admin/recordings')
  const row = table.getByRole('row').filter({ has: page.locator(`a[href$="${recordingId}"]`) })
  await expect(row.getByText('Incomplete')).toBeVisible()

  await page.goto(`/admin/recordings/${recordingId}`)
  await expect(page.getByRole('heading', { name: 'This recording failed validation.' })).toBeVisible()
})

test('deleting a recording waits for confirmation', async () => {
  await page.goto('/admin/recordings')
  const table = page.getByRole('table', { name: 'Recordings' })
  const complete = table.getByRole('row').filter({ has: page.getByText('Complete', { exact: true }) })
  await complete.getByRole('link', { name: /^Play recording/ }).first().click()
  await page.getByRole('button', { name: 'Delete recording' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByRole('alertdialog')).toBeHidden()
  await expect(page.getByRole('button', { name: 'Delete recording' })).toBeVisible()
  await page.getByRole('button', { name: 'Delete recording' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Delete' }).click()
  await expect(page.getByText('Recording deleted.')).toBeVisible()
})

test('repeated wrong passwords are throttled, even the right one, until the window passes', async ({ browser }) => {
  test.setTimeout(180_000)
  const context = await browser.newContext()
  const p = await context.newPage()
  collectConsoleProblems(p, consoleProblems, 'sign-in')
  await p.goto('/admin')
  const throttled = p.getByText(/^Too many attempts\. Try again in \d+ seconds\.$/)
  const attempt = async (password: string) => {
    await p.getByLabel('Administrator password').fill(password)
    const [response] = await Promise.all([
      p.waitForResponse((r) => r.url().endsWith('/api/v1/admin/login')),
      p.getByRole('button', { name: 'Sign in' }).click(),
    ])
    return response
  }
  // Earlier sign-ins share this client's throttle window. A refusal that is
  // about to expire is waited out so the next one starts a fresh window.
  let refused: Awaited<ReturnType<typeof attempt>> | undefined
  for (let i = 0; i < 30 && !refused; i++) {
    const response = await attempt(`wrong ${i}`)
    if (response.status() !== 429) {
      expect(response.status()).toBe(401)
      continue
    }
    const wait = Number(await response.headerValue('retry-after'))
    expect(wait, 'Retry-After on the throttled answer').toBeGreaterThan(0)
    if (wait >= 20) refused = response
    else await p.waitForTimeout((wait + 1) * 1000)
  }
  expect(refused, 'sign-in was throttled').toBeDefined()
  await expect(throttled).toBeVisible()
  expect((await attempt('e2e correct horse battery')).status()).toBe(429)
  await expect(throttled).toBeVisible()
  await expect(p.getByRole('navigation', { name: 'Admin' })).toBeHidden()

  await signIn(p)
  await p.goto('/admin/audit')
  await expect(p.getByRole('table', { name: 'Audit events' }).getByText('admin.login.throttled').first()).toBeVisible()
  await context.close()
})

test('no console errors or warnings were logged', () => {
  // The browser itself logs refused sign-ins (401/429), the damaged recording (422), and requests made while offline.
  const expected = [
    /status of (401|429).*\/api\/v1\/admin\/login/,
    /status of 422.*\/api\/v1\/admin\/recordings\/[^/]+\/events/,
    /ERR_INTERNET_DISCONNECTED|network connection was lost|Internet connection appears to be offline/i,
  ]
  const unexpected = consoleProblems.filter((p) => !expected.some((e) => e.test(p)))
  expect(unexpected).toEqual([])
})
