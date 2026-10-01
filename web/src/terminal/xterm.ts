import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

/** The slice of xterm.js the workspace and player use. */
export interface XtermHandle {
  readonly cols: number
  readonly rows: number
  /** Queues data for parsing; `done` runs once it has been parsed. */
  write(data: Uint8Array | string, done?: () => void): void
  /** Applies immediately, ahead of any data still waiting to be parsed. */
  reset(): void
  resize(cols: number, rows: number): void
  /** The size that would fill the container, without resizing. */
  proposeFit(): { cols: number; rows: number } | null
  setInputEnabled(enabled: boolean): void
  /** Whether Tab can move focus onto the terminal. */
  setTabStop(enabled: boolean): void
  describeBy(id: string | null): void
  /** Returning false lets the browser handle the key instead of the terminal. */
  setKeyHandler(handler: (event: KeyboardEvent) => boolean): void
  onData(listener: (data: string) => void): () => void
  focus(): void
  dispose(): void
}

export type XtermFactory = (container: HTMLElement, options?: { label?: string }) => XtermHandle

export const terminalTheme = {
  background: '#07090d',
  foreground: '#e8eef6',
  cursor: '#e8eef6',
  cursorAccent: '#07090d',
  selectionBackground: '#2c3d55',
  black: '#1b2330',
  red: '#f06a6a',
  green: '#7fd48a',
  yellow: '#f0b45a',
  blue: '#6ea8fe',
  magenta: '#9a7cff',
  cyan: '#45d4e6',
  white: '#c9d3df',
  brightBlack: '#56657a',
  brightRed: '#ff8e8e',
  brightGreen: '#a2e6ab',
  brightYellow: '#ffcb80',
  brightBlue: '#94c0ff',
  brightMagenta: '#b9a3ff',
  brightCyan: '#7be3f0',
  brightWhite: '#f4f7fb',
}

export const createXterm: XtermFactory = (container, options = {}) => {
  const terminal = new Terminal({
    theme: terminalTheme,
    fontFamily: "'IBM Plex Mono', ui-monospace, Menlo, monospace",
    fontSize: 14,
    lineHeight: 1.15,
    cursorBlink: false,
    scrollback: 5000,
    allowProposedApi: false,
    screenReaderMode: false,
  })
  const fit = new FitAddon()
  terminal.loadAddon(fit)
  terminal.open(container)
  if (options.label) terminal.textarea?.setAttribute('aria-label', options.label)

  return {
    get cols() {
      return terminal.cols
    },
    get rows() {
      return terminal.rows
    },
    write: (data, done) => terminal.write(data, done),
    reset: () => terminal.reset(),
    resize: (cols, rows) => {
      if (cols !== terminal.cols || rows !== terminal.rows) terminal.resize(cols, rows)
    },
    proposeFit: () => {
      const size = fit.proposeDimensions()
      if (!size || !Number.isFinite(size.cols) || !Number.isFinite(size.rows)) return null
      return { cols: Math.max(2, size.cols), rows: Math.max(1, size.rows) }
    },
    setInputEnabled: (enabled) => {
      terminal.options.disableStdin = !enabled
      terminal.options.cursorStyle = enabled ? 'block' : 'underline'
    },
    setTabStop: (enabled) => {
      if (terminal.textarea) terminal.textarea.tabIndex = enabled ? 0 : -1
    },
    describeBy: (id) => {
      if (id) terminal.textarea?.setAttribute('aria-describedby', id)
      else terminal.textarea?.removeAttribute('aria-describedby')
    },
    setKeyHandler: (handler) => terminal.attachCustomKeyEventHandler(handler),
    onData: (listener) => {
      const disposable = terminal.onData(listener)
      return () => disposable.dispose()
    },
    focus: () => terminal.focus(),
    dispose: () => terminal.dispose(),
  }
}
