import { RenderQueue } from './renderQueue'
import type { XtermHandle } from './xterm'

/**
 * Puts a render queue in front of an xterm and mirrors its state onto the
 * screen element: `data-render` is "busy" while output is still being parsed,
 * and `data-cols`/`data-rows` give the size actually applied.
 */
export function createScreenQueue(xterm: XtermHandle, element: HTMLElement) {
  element.dataset.render = 'idle'
  const mirrorSize = () => {
    element.dataset.cols = String(xterm.cols)
    element.dataset.rows = String(xterm.rows)
  }
  mirrorSize()
  return new RenderQueue(
    {
      write: (data, done) => xterm.write(data, done),
      reset: () => xterm.reset(),
      resize: (cols, rows) => {
        xterm.resize(cols, rows)
        mirrorSize()
      },
    },
    { onIdleChange: (idle) => (element.dataset.render = idle ? 'idle' : 'busy') },
  )
}
