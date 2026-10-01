export const editableHint = 'Press Escape, then Tab, to move focus out of the terminal.'
export const readOnlyHint = 'Tab moves focus past the terminal.'

const modifierKeys = new Set(['Shift', 'Control', 'Alt', 'Meta'])

/**
 * Builds an xterm custom key handler. Returning false makes xterm ignore the
 * key without preventing its default, so the browser moves focus. A terminal
 * that cannot be typed into never keeps Tab; an editable one keeps Tab for
 * the shell unless the key just before it was Escape (which still reaches
 * the shell). Option+Tab counts as Tab.
 */
export function createFocusEscape(isEditable: () => boolean) {
  let armed = false
  return (event: KeyboardEvent): boolean => {
    if (event.type !== 'keydown') return true
    // Safari moves focus with Option+Tab unless full keyboard access is on.
    const focusTab = event.key === 'Tab' && !event.ctrlKey && !event.metaKey
    if (focusTab) {
      const leave = armed || !isEditable()
      armed = false
      return !leave
    }
    if (modifierKeys.has(event.key)) return true
    armed = event.key === 'Escape'
    return true
  }
}
