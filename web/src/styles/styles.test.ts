// @vitest-environment node
/// <reference types="node" />
import { readFileSync } from 'node:fs'

import { expect, test } from 'vitest'

const app = readFileSync(new URL('./app.css', import.meta.url), 'utf8')

function rule(css: string, selector: string) {
  const start = css.indexOf(`\n${selector} {`)
  expect(start, `${selector} is styled`).toBeGreaterThanOrEqual(0)
  return css.slice(start, css.indexOf('}', start))
}

test('the keyboard hint under the terminal is prose, set in the sans face', () => {
  expect(rule(app, '.terminal-hint')).toContain('font-family: var(--font-sans)')
})
