/// <reference types="node" />
import { DatabaseSync } from 'node:sqlite'

import AxeBuilder from '@axe-core/playwright'
import { expect, type Page, test } from '@playwright/test'

import { dataDir } from './servers'

export const adminPassword = 'e2e correct horse battery'

export const readout = (p: Page) => p.getByTestId('readout')
export const screenRows = (p: Page) => p.locator('.xterm-rows')

/**
 * Signs in, completing first-run setup if this spec runs on a fresh server.
 * Sign-in is throttled per address, so a run that just tripped the limit
 * waits for the window to pass instead of failing.
 */
export async function signIn(page: Page) {
  await page.goto('/admin')
  const attempt = async (password: string) => {
    await page.getByLabel('Administrator password').fill(password)
    await page.getByRole('button', { name: 'Sign in' }).click()
    return Promise.race([
      page
        .getByRole('navigation', { name: 'Admin' })
        .waitFor()
        .then(() => 'signed-in' as const),
      page
        .getByLabel('New password', { exact: true })
        .waitFor()
        .then(() => 'change-password' as const),
      page
        .getByText('That password is not correct.')
        .waitFor()
        .then(() => 'wrong' as const),
      page
        .getByText(/^Too many attempts/)
        .waitFor()
        .then(() => 'throttled' as const),
    ])
  }
  const until = async (password: string) => {
    const deadline = Date.now() + 90_000
    for (;;) {
      const outcome = await attempt(password)
      if (outcome !== 'throttled' || Date.now() > deadline) return outcome
      await page.waitForTimeout(5_000)
    }
  }
  let outcome = await until(adminPassword)
  if (outcome === 'wrong') outcome = await until('CHANGEME')
  if (outcome === 'change-password') {
    await page.getByLabel('New password', { exact: true }).fill(adminPassword)
    await page.getByLabel('Confirm new password').fill(adminPassword)
    await page.getByRole('button', { name: 'Save password' }).click()
  }
  await expect(page.getByRole('navigation', { name: 'Admin' })).toBeVisible()
}

export async function startTerminal(page: Page) {
  await page.goto('/admin/sessions')
  await page.getByRole('button', { name: 'New terminal' }).click()
  await page.getByRole('button', { name: 'Start terminal' }).click()
  await expect(readout(page)).toContainText('Live')
}

/** Waits until the terminal has parsed everything queued for it and the screen stops changing. */
export async function settled(page: Page, timeout = 30_000) {
  const screen = page.locator('.workspace__screen').first()
  let previous: string | null = null
  await expect(async () => {
    const render = await screen.getAttribute('data-render')
    const text = await screenRows(page).innerText()
    const same = text === previous && render === 'idle'
    previous = render === 'idle' ? text : null
    expect(same).toBe(true)
  }).toPass({ timeout, intervals: [250] })
  return previous!
}

export function collectConsoleProblems(
  page: Page,
  sink: string[],
  who: string,
) {
  // WebKit reports a same-origin request cancelled by navigation this way.
  const cancelledByNavigation = (text: string) => {
    const cancelled = /(https?:\/\/\S+) due to access control checks/.exec(text)
    return (
      cancelled !== null &&
      new URL(cancelled[1]).origin === new URL(page.url()).origin
    )
  }
  page.on('console', (message) => {
    if (message.type() !== 'error' && message.type() !== 'warning') return
    if (
      /status of 401/.test(message.text()) &&
      /\/api\/v1\/(admin|access)\/session$/.test(message.location().url)
    )
      return
    if (cancelledByNavigation(message.text())) return
    sink.push(`${who}: ${message.text()} (${message.location().url})`)
  })
  page.on('pageerror', (error) => {
    // Playwright's WebKit adapter can raise that same report as a page error,
    // split at its first colon into name ("… http") and message ("//host/…"
    // minus one character), so the URL is put back together before checking.
    if (cancelledByNavigation(`${error.name}:/${error.message}`)) return
    sink.push(`${who}: ${error.message}`)
  })
}

/** The element that has keyboard focus, described by role-ish name for assertions. */
export async function focused(page: Page) {
  return page.evaluate(() => {
    const el = document.activeElement as HTMLElement | null
    if (!el) return ''
    if (el.classList.contains('xterm-helper-textarea')) return 'terminal'
    const label =
      el.getAttribute('aria-label') ??
      (
        (el as HTMLInputElement).labels?.[0]?.textContent ??
        el.textContent ??
        ''
      ).trim()
    return `${el.tagName.toLowerCase()}:${label}`
  })
}

/** Creates a share link from the owner workspace rail and returns its URL. */
export async function createLink(
  page: Page,
  role: 'Viewer' | 'Editor',
  label: string,
) {
  const rail = page.getByRole('complementary', { name: 'Terminal details' })
  await rail.getByLabel(role, { exact: true }).check()
  await rail.getByLabel('Label').fill(label)
  await rail.getByRole('button', { name: 'Create link' }).click()
  const url = await rail.getByLabel(`${role} link`).inputValue()
  await rail.getByRole('button', { name: 'Done' }).click()
  return url
}

const wcagTags = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']

/** Runs axe's WCAG 2.2 A/AA rules, including color contrast, and fails on any violation. */
export async function expectAccessible(page: Page, where: string) {
  const results = await new AxeBuilder({ page }).withTags(wcagTags).analyze()
  const violations = results.violations.map(
    (v) =>
      `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`,
  )
  expect(violations, `${where}: axe violations`).toEqual([])
  const contrast = [...results.passes, ...results.incomplete].find(
    (r) => r.id === 'color-contrast',
  )
  expect(contrast, `${where}: color contrast was evaluated`).toBeDefined()
  return results
}

/**
 * Describes what is focused and whether focus is visible: the element, or
 * the control or terminal frame around it, draws an outline.
 */
export async function focusStop(page: Page) {
  return page.evaluate(() => {
    const el = document.activeElement as HTMLElement | null
    if (!el || el === document.body) return { name: 'body', visible: false }
    const terminal = el.classList.contains('xterm-helper-textarea')
    const label =
      el.getAttribute('aria-label') ??
      (
        (el as HTMLInputElement).labels?.[0]?.textContent ??
        el.textContent ??
        ''
      ).trim()
    const name = terminal
      ? 'terminal'
      : `${el.tagName.toLowerCase()}:${label.slice(0, 60)}`
    // An outline on a transparent element (such as xterm's hidden input) cannot be seen.
    const outlined = (node: Element) => {
      const style = getComputedStyle(node)
      if (parseFloat(style.opacity) === 0) return false
      return (
        style.outlineStyle !== 'none' &&
        parseFloat(style.outlineWidth) >= 2 &&
        style.outlineColor !== 'rgba(0, 0, 0, 0)'
      )
    }
    let node: Element | null = el
    for (let depth = 0; node && depth < 6; depth++, node = node.parentElement) {
      if (outlined(node)) return { name, visible: true }
    }
    return { name, visible: false }
  })
}

/**
 * The key that moves focus to every control. WebKit on macOS follows the
 * system default, where Tab reaches only text fields and Option+Tab reaches
 * everything (as in Safari).
 */
export function tabKey(back = false) {
  const option =
    test.info().project.name === 'webkit' && process.platform === 'darwin'
  return `${option ? 'Alt+' : ''}${back ? 'Shift+' : ''}Tab`
}

/**
 * Tabs through the page from the top until it reaches the stop whose
 * description matches target, checking every stop on the way shows focus.
 */
export async function tabTo(page: Page, target: RegExp, maxStops = 60) {
  const stops: string[] = []
  for (let i = 0; i < maxStops; i++) {
    await page.keyboard.press(tabKey())
    const stop = await focusStop(page)
    // Between the last control and the first, focus passes through the browser itself.
    if (stop.name === 'body') continue
    stops.push(stop.name)
    expect(stop.visible, `focus is visible on ${stop.name}`).toBe(true)
    if (target.test(stop.name)) return stops
  }
  throw new Error(
    `never reached ${target} by Tab; stops were: ${stops.join(', ')}`,
  )
}

/** Fails if anything on the page still moves when the user asked for reduced motion. */
export async function expectNoMotion(page: Page, where: string) {
  const moving = await page.evaluate(() => {
    const seconds = (value: string) =>
      Math.max(
        ...value
          .split(',')
          .map((v) => parseFloat(v) * (v.trim().endsWith('ms') ? 0.001 : 1)),
      )
    const found: string[] = []
    for (const el of Array.from(document.querySelectorAll('*'))) {
      const style = getComputedStyle(el)
      const transition =
        seconds(style.transitionDuration) > 0.01 &&
        style.transitionProperty !== 'none'
      const animation =
        style.animationName !== 'none' &&
        seconds(style.animationDuration) > 0.01
      if (transition || animation)
        found.push(
          `${el.tagName.toLowerCase()}.${String(el.className).split(' ')[0]}`,
        )
    }
    return found
  })
  expect(
    moving,
    `${where}: elements still animating under reduced motion`,
  ).toEqual([])
}

/**
 * Overwrites the stored chunk checksums of a recording, the way disk damage
 * would: below the schema, so the trigger that keeps chunks immutable is
 * set aside for the one write and restored in the same transaction.
 */
export function damageRecording(baseURL: string, recordingId: string) {
  const db = new DatabaseSync(
    `${dataDir(Number(new URL(baseURL).port))}/webpty.db`,
  )
  try {
    db.exec('PRAGMA busy_timeout = 5000')
    const trigger = db
      .prepare(
        "SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'recording_chunks_are_immutable'",
      )
      .get() as { sql: string } | undefined
    expect(trigger, 'the immutability trigger exists').toBeDefined()
    db.exec('BEGIN IMMEDIATE')
    try {
      db.exec('DROP TRIGGER recording_chunks_are_immutable')
      const result = db
        .prepare(
          'UPDATE recording_chunks SET checksum = zeroblob(32) WHERE recording_id = (SELECT id FROM recordings WHERE public_id = ?)',
        )
        .run(recordingId)
      expect(Number(result.changes), 'chunks damaged').toBeGreaterThan(0)
      db.exec(trigger!.sql)
      db.exec('COMMIT')
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    }
  } finally {
    db.close()
  }
}
