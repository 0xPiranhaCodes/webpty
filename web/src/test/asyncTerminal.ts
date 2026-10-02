import type { WritableTerminal } from '../terminal/renderQueue'

/**
 * Behaves like xterm.js where it matters for ordering: written data is queued
 * and parsed later, while reset and resize take effect immediately, ahead of
 * anything still queued. Writing past the watermark throws, as xterm does.
 */
export class AsyncTerminal implements WritableTerminal {
  text = ''
  cols = 80
  rows = 24
  /** What the screen looked like as each piece of data was parsed. */
  parsedAt: string[] = []
  writes = 0
  maxPendingBytes = 0
  private pending: { data: Uint8Array | string; done?: () => void }[] = []
  private pendingBytes = 0

  constructor(
    private readonly options: { autoDrain?: boolean; watermark?: number } = {},
  ) {}

  write(data: Uint8Array | string, done?: () => void) {
    if (this.pendingBytes > (this.options.watermark ?? 50_000_000)) {
      throw new Error(
        'write data discarded, use flow control to avoid losing data',
      )
    }
    this.writes++
    this.pending.push({ data, done })
    this.pendingBytes += data.length
    this.maxPendingBytes = Math.max(this.maxPendingBytes, this.pendingBytes)
    if (this.options.autoDrain !== false) queueMicrotask(() => this.drainOne())
  }

  reset() {
    this.text = ''
  }

  resize(cols: number, rows: number) {
    this.cols = cols
    this.rows = rows
  }

  get queued() {
    return this.pending.length
  }

  /** Parses the oldest queued write and runs its callback. */
  drainOne() {
    const next = this.pending.shift()
    if (!next) return false
    this.pendingBytes -= next.data.length
    this.text +=
      typeof next.data === 'string'
        ? next.data
        : new TextDecoder().decode(next.data)
    this.parsedAt.push(`${this.cols}x${this.rows}`)
    next.done?.()
    return true
  }

  drainAll() {
    while (this.drainOne());
  }
}
