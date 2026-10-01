import type { WebSocketLike } from '../terminal/connection'
import type { XtermHandle } from '../terminal/xterm'
import { AsyncTerminal } from './asyncTerminal'

export class FakeSocket implements WebSocketLike {
  static all: FakeSocket[] = []
  readyState = 0
  sent: string[] = []
  onopen: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onclose: ((event: { code: number; reason: string }) => void) | null = null
  onerror: (() => void) | null = null

  constructor(readonly url: string) {
    FakeSocket.all.push(this)
  }
  static last() {
    return FakeSocket.all.at(-1)!
  }
  send(data: string) {
    this.sent.push(data)
  }
  close() {
    this.readyState = 3
  }
  open() {
    this.readyState = 1
    this.onopen?.()
  }
  server(message: unknown) {
    this.onmessage?.({ data: JSON.stringify(message) })
  }
  serverClose(code: number, reason = '') {
    this.readyState = 3
    this.onclose?.({ code, reason })
  }
}

/**
 * An xterm stand-in with xterm's timing: writes are parsed later (on a
 * microtask, or only when drained if `FakeXterm.manual` is set), while reset
 * and resize apply at once.
 */
export class FakeXterm extends AsyncTerminal implements XtermHandle {
  static all: FakeXterm[] = []
  static manual = false
  inputEnabled = true
  tabStop = true
  describedBy: string | null = null
  resets = 0
  keyHandler: ((event: KeyboardEvent) => boolean) | null = null
  private dataListeners: ((data: string) => void)[] = []

  readonly label?: string

  constructor(options: { label?: string } = {}) {
    super({ autoDrain: !FakeXterm.manual })
    this.label = options.label
    FakeXterm.all.push(this)
  }
  static last() {
    return FakeXterm.all.at(-1)!
  }
  /** Everything parsed since the last reset. */
  get written() {
    return this.text
  }
  reset() {
    super.reset()
    this.resets++
  }
  proposeFit() {
    return { cols: 120, rows: 32 }
  }
  setInputEnabled(enabled: boolean) {
    this.inputEnabled = enabled
  }
  setTabStop(enabled: boolean) {
    this.tabStop = enabled
  }
  describeBy(id: string | null) {
    this.describedBy = id
  }
  setKeyHandler(handler: (event: KeyboardEvent) => boolean) {
    this.keyHandler = handler
  }
  onData(listener: (data: string) => void) {
    this.dataListeners.push(listener)
    return () => {
      this.dataListeners = this.dataListeners.filter((l) => l !== listener)
    }
  }
  type(data: string) {
    this.dataListeners.forEach((l) => l(data))
  }
  focus() {}
  dispose() {}
}

export function resetTerminalFakes() {
  FakeSocket.all = []
  FakeXterm.all = []
  FakeXterm.manual = false
}
