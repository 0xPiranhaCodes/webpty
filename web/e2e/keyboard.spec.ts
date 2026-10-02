import {
  expect,
  test,
  type Browser,
  type BrowserContext,
  type Page,
} from '@playwright/test'

import {
  collectConsoleProblems,
  focused,
  readout,
  screenRows,
  signIn,
  startTerminal,
  tabKey,
} from './support'

// Everything after setup is driven by the keyboard alone.
test.describe.configure({ mode: 'serial' })

const problems: string[] = []

async function tabTo(
  page: Page,
  target: string | RegExp,
  { back = false, max = 40 } = {},
) {
  const seen: string[] = []
  for (let i = 0; i < max; i++) {
    await page.keyboard.press(tabKey(back))
    const now = await focused(page)
    seen.push(now)
    if (typeof target === 'string' ? now === target : target.test(now))
      return seen
  }
  throw new Error(
    `focus never reached ${target}; went through:\n${seen.join('\n')}`,
  )
}

async function describedBy(page: Page) {
  return page.locator('.xterm-helper-textarea').evaluate((el) =>
    (el.getAttribute('aria-describedby') ?? '')
      .split(/\s+/)
      .filter(Boolean)
      .map((id) => document.getElementById(id)?.textContent ?? '')
      .join(' '),
  )
}

async function guest(browser: Browser, url: string) {
  const context = await browser.newContext()
  const page = await context.newPage()
  collectConsoleProblems(page, problems, 'guest')
  await page.goto(url)
  return { context, page }
}

let ownerContext: BrowserContext
let owner: Page

test.beforeAll(async ({ browser }) => {
  ownerContext = await browser.newContext()
  owner = await ownerContext.newPage()
  await signIn(owner)
  collectConsoleProblems(owner, problems, 'owner')
  await startTerminal(owner)
})

// The browser is shared with the specs that follow; a page left open here
// keeps rendering and slows every later test.
test.afterAll(async () => {
  await ownerContext.close()
})

test('owner: Tab stays in the terminal, Escape then Tab leaves it, and sharing works by keyboard', async () => {
  await owner.locator('.xterm-helper-textarea').focus()
  await expect(
    owner.getByText(
      'Press Escape, then Tab, to move focus out of the terminal.',
    ),
  ).toBeVisible()
  expect(await describedBy(owner)).toContain(
    'Press Escape, then Tab, to move focus out of the terminal.',
  )

  await owner.keyboard.type('echo kb-owner')
  await owner.keyboard.press(tabKey())
  expect(await focused(owner)).toBe('terminal')
  await owner.keyboard.press(tabKey(true))
  expect(await focused(owner)).toBe('terminal')
  await owner.keyboard.press('Enter')
  await expect(screenRows(owner)).toContainText('kb-owner')

  // Escape still reaches the shell; only Escape followed by Tab moves focus.
  await owner.keyboard.press('Escape')
  await owner.keyboard.press(tabKey())
  expect(await focused(owner)).not.toBe('terminal')
  await tabTo(owner, 'input:Label')
  await owner.keyboard.type('Keyboard viewer')
  await tabTo(owner, 'button:Create link')
  await owner.keyboard.press('Enter')
  const rail = owner.getByRole('complementary', { name: 'Terminal details' })
  const url = await rail.getByLabel('Viewer link').inputValue()
  expect(url).toMatch(/\/join#token=/)
  await tabTo(owner, 'button:Done')
  await owner.keyboard.press('Enter')
  test.info().annotations.push({ type: 'viewer-url', description: url })
  ;(globalThis as { viewerUrl?: string }).viewerUrl = url

  // Back into the terminal and out the other way, to the header controls.
  await tabTo(owner, 'terminal', { back: true })
  await owner.keyboard.press('Escape')
  await tabTo(owner, 'button:Terminate', { back: true })
})

test('viewer: Tab moves straight through the read-only terminal', async ({
  browser,
}) => {
  const viewer = await guest(
    browser,
    (globalThis as { viewerUrl?: string }).viewerUrl!,
  )
  await expect(viewer.page.getByText('You joined as a viewer.')).toBeVisible()
  await expect(readout(viewer.page)).toContainText('Live')

  await tabTo(viewer.page, 'terminal')
  await expect(
    viewer.page.getByText('Tab moves focus past the terminal.'),
  ).toBeVisible()
  expect(await describedBy(viewer.page)).toContain(
    'Tab moves focus past the terminal.',
  )
  await viewer.page.keyboard.press(tabKey())
  expect(await focused(viewer.page)).not.toBe('terminal')
  await tabTo(viewer.page, 'terminal', { back: true })
  await tabTo(viewer.page, 'button:Leave', { back: true })

  // The owner revokes the viewer from the keyboard.
  await owner.locator('.xterm-helper-textarea').focus()
  await owner.keyboard.press('Escape')
  await owner.keyboard.press(tabKey())
  await tabTo(owner, 'button:Revoke Keyboard viewer')
  await owner.keyboard.press('Enter')
  const dialog = owner.getByRole('alertdialog')
  await expect(dialog).toBeVisible()
  await tabTo(owner, 'button:Revoke')
  await owner.keyboard.press('Enter')
  await expect(readout(viewer.page)).toHaveText('Access revoked')
  await viewer.context.close()
})

test('playback: transport controls are reachable and the terminal is skipped', async () => {
  await owner.locator('.xterm-helper-textarea').focus()
  await owner.keyboard.press('Escape')
  await tabTo(owner, 'button:Terminate', { back: true })
  await owner.keyboard.press('Enter')
  await tabTo(owner, 'button:Terminate')
  await owner.keyboard.press('Enter')
  await expect(readout(owner)).toHaveText(/Terminated|Ended/)

  await owner.goto('/admin/recordings')
  await expect(async () => {
    await owner.reload()
    await expect(
      owner
        .getByRole('table', { name: 'Recordings' })
        .locator('tbody tr')
        .first(),
    ).toContainText('Complete', { timeout: 1000 })
  }).toPass({ timeout: 30_000 })
  await owner
    .getByRole('link', { name: /^Play recording/ })
    .first()
    .focus()
  await owner.keyboard.press('Enter')
  await expect(readout(owner)).toContainText('Playback')
  await expect(owner.getByRole('slider', { name: 'Position' })).toBeVisible()

  const order = await tabTo(owner, 'button:Play')
  expect(order).not.toContain('terminal')
  expect(order).toEqual(
    expect.arrayContaining([
      'a:Export as asciicast',
      'button:Delete recording',
    ]),
  )
  await owner.keyboard.press('Space')
  await expect(readout(owner)).toContainText('playing')
  await owner.keyboard.press('Space')
  await expect(readout(owner)).toContainText('paused')

  await owner.keyboard.press(tabKey())
  expect(await focused(owner)).toBe('input:Position')
  await owner.keyboard.press('End')
  const slider = owner.getByRole('slider', { name: 'Position' })
  const total = (await slider.getAttribute('aria-valuetext'))!.split(' of ')[1]
  await expect(slider).toHaveAttribute('aria-valuetext', `${total} of ${total}`)
  await expect(screenRows(owner)).toContainText('kb-owner')
  await owner.keyboard.press('Home')
  await expect(slider).toHaveAttribute('aria-valuetext', `0:00 of ${total}`)

  await tabTo(owner, /^input:(0\.5|1|1\.5|2)×$/)
  await owner.keyboard.press('ArrowRight')
  expect(await focused(owner)).toMatch(/×$/)
  // Leaving the transport backwards returns to the slider without visiting the terminal.
  const back = await tabTo(owner, 'input:Position', { back: true })
  expect(back).not.toContain('terminal')

  expect(problems).toEqual([])
})
