import { expect, test } from '@playwright/test'

import {
  collectConsoleProblems,
  readout,
  screenRows,
  settled,
  signIn,
  startTerminal,
} from './support'

// Real xterm parses writes asynchronously. These tests check what the real
// terminal buffer shows after the kinds of seeking a person actually does.
test.describe.configure({ mode: 'serial' })

const lines = 300_000 // about 2 MB of output per burst

test('rapid seeks, slider drags, and recorded resizes leave exactly the right screen', async ({
  page,
}) => {
  const problems: string[] = []
  await signIn(page)
  collectConsoleProblems(page, problems, 'admin')
  await startTerminal(page)

  await page.locator('.xterm').click()
  await page.keyboard.type(`seq 1 ${lines}; echo FIRST-DONE\n`)
  await expect(screenRows(page)).toContainText('FIRST-DONE', {
    timeout: 60_000,
  })
  await settled(page)

  // A recorded resize between two bursts of output.
  await page.setViewportSize({ width: 960, height: 700 })
  await page.waitForTimeout(500)
  await page.keyboard.type(`clear; echo NARROW-$(tput cols)\n`)
  await expect(screenRows(page)).toContainText(/NARROW-\d+/)
  const narrowCols = /NARROW-(\d+)/.exec(await screenRows(page).innerText())![1]
  await page.keyboard.type(`seq 1 ${lines}; echo END-MARK\n`)
  await expect(screenRows(page)).toContainText('END-MARK', { timeout: 60_000 })
  await settled(page)

  await page.getByRole('button', { name: 'Terminate', exact: true }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: 'Terminate' })
    .click()
  await expect(readout(page)).toHaveText(/Terminated|Ended/)

  await page.goto('/admin/recordings')
  const newest = page.getByRole('link', { name: /^Play recording/ }).first()
  await expect(async () => {
    await page.reload()
    await expect(
      page
        .getByRole('table', { name: 'Recordings' })
        .locator('tbody tr')
        .first(),
    ).toContainText('Complete', { timeout: 1000 })
  }).toPass({ timeout: 30_000 })
  await newest.click()
  await expect(readout(page)).toContainText('Playback')
  await expect(page.getByRole('slider', { name: 'Position' })).toBeVisible({
    timeout: 30_000,
  })

  const slider = page.getByRole('slider', { name: 'Position' })
  await slider.focus()

  // Many seeks in quick succession, each a full replay of megabytes of output.
  for (let i = 0; i < 20; i++) {
    await page.keyboard.press('End')
    await page.keyboard.press('Home')
  }
  await page.keyboard.press('End')
  const atEnd = (await settled(page))
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)
  const endIndex = atEnd.findIndex((l) => l === 'END-MARK')
  expect(endIndex, atEnd.join('\n')).toBeGreaterThan(0)
  // Nothing from an older seek generation is appended after the final screen.
  expect(
    atEnd.slice(endIndex + 1).every((l) => /^\S*\$$/.test(l)),
    atEnd.slice(endIndex).join('\n'),
  ).toBe(true)
  // The output above the marker is the recorded tail, in order, without interleaving.
  const tail = atEnd
    .slice(0, endIndex)
    .filter((l) => /^\d+$/.test(l))
    .map(Number)
  expect(tail.length).toBeGreaterThan(5)
  expect(tail.at(-1)).toBe(lines)
  tail.forEach((n, i) => i > 0 && expect(n).toBe(tail[i - 1] + 1))
  // The recorded resize was applied after the output before it, and stuck.
  expect(
    await page.locator('.workspace__screen').getAttribute('data-cols'),
  ).toBe(narrowCols)

  await page.keyboard.press('Home')
  expect((await settled(page)).trim()).toBe('')

  // Dragging the slider issues a stream of seeks; the screen must end coherent.
  const box = (await slider.boundingBox())!
  await page.mouse.move(box.x + 2, box.y + box.height / 2)
  await page.mouse.down()
  for (let step = 0; step <= 40; step++) {
    await page.mouse.move(
      box.x + 2 + ((box.width - 4) * (step % 2 === 0 ? step : 40 - step)) / 40,
      box.y + box.height / 2,
    )
  }
  await page.mouse.move(box.x + box.width - 1, box.y + box.height / 2)
  await page.mouse.up()
  const dragged = (await settled(page))
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)
  expect(dragged).toContain('END-MARK')

  expect(problems.filter((p) => /discarded/.test(p))).toEqual([])
  expect(problems).toEqual([])
})
