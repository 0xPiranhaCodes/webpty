import { describe, expect, test } from 'vitest'

import { createFocusEscape } from './focusEscape'

const key = (k: string, init: KeyboardEventInit & { type?: string } = {}) =>
  new KeyboardEvent(init.type ?? 'keydown', { key: k, ...init })

/** Returns true when the terminal should handle the key, false when the browser should. */
function handler(editable: boolean) {
  let state = editable
  const handle = createFocusEscape(() => state)
  return { handle, setEditable: (v: boolean) => (state = v) }
}

describe('focus escape', () => {
  test('a read-only terminal lets Tab and Shift+Tab move focus', () => {
    const { handle } = handler(false)
    expect(handle(key('Tab'))).toBe(false)
    expect(handle(key('Tab', { shiftKey: true }))).toBe(false)
    expect(handle(key('a'))).toBe(true)
  })

  test('Option+Tab, the Safari focus key, moves focus the same way Tab does', () => {
    const readOnly = handler(false)
    expect(readOnly.handle(key('Tab', { altKey: true }))).toBe(false)
    expect(readOnly.handle(key('Tab', { altKey: true, shiftKey: true }))).toBe(
      false,
    )

    const editable = handler(true)
    expect(editable.handle(key('Tab', { altKey: true }))).toBe(true)
    expect(editable.handle(key('Escape'))).toBe(true)
    expect(editable.handle(key('Alt'))).toBe(true)
    expect(editable.handle(key('Tab', { altKey: true }))).toBe(false)
  })

  test('an editable terminal keeps Tab for the shell', () => {
    const { handle } = handler(true)
    expect(handle(key('Tab'))).toBe(true)
    expect(handle(key('Tab', { shiftKey: true }))).toBe(true)
  })

  test('Escape then Tab leaves an editable terminal, and Escape still reaches the shell', () => {
    const { handle } = handler(true)
    expect(handle(key('Escape'))).toBe(true)
    expect(handle(key('Escape', { type: 'keyup' }))).toBe(true)
    expect(handle(key('Tab'))).toBe(false)
    expect(handle(key('Tab'))).toBe(true)
  })

  test('Escape then Shift+Tab leaves backwards', () => {
    const { handle } = handler(true)
    handle(key('Escape'))
    expect(handle(key('Shift', { shiftKey: true }))).toBe(true)
    expect(handle(key('Tab', { shiftKey: true }))).toBe(false)
  })

  test('any other key after Escape cancels the escape', () => {
    const { handle } = handler(true)
    handle(key('Escape'))
    handle(key('b'))
    expect(handle(key('Tab'))).toBe(true)
  })

  test('Control+Tab and Command+Tab are always the terminal’s', () => {
    const { handle } = handler(false)
    expect(handle(key('Tab', { ctrlKey: true }))).toBe(true)
    expect(handle(key('Tab', { metaKey: true }))).toBe(true)
  })

  test('follows permission changes', () => {
    const { handle, setEditable } = handler(true)
    expect(handle(key('Tab'))).toBe(true)
    setEditable(false)
    expect(handle(key('Tab'))).toBe(false)
  })
})
