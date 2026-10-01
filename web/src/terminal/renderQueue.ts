/** The parts of a terminal whose ordering the queue controls. */
export interface WritableTerminal {
  /** Queues data; `done` runs once the terminal has parsed it. */
  write(data: Uint8Array, done: () => void): void
  /** Takes effect immediately, ahead of any data the terminal still holds. */
  reset(): void
  resize(cols: number, rows: number): void
}

export interface RenderQueueOptions {
  /** Largest write handed to the terminal; at most one is unparsed at a time. */
  maxChunkBytes?: number
  onIdleChange?: (idle: boolean) => void
}

type Op =
  | { kind: 'write'; parts: Uint8Array[]; size: number }
  | { kind: 'resize'; cols: number; rows: number }
  | { kind: 'reset'; cols: number; rows: number }

/**
 * Orders output, resizes, and resets for a terminal that parses writes
 * asynchronously. Output is coalesced and fed one bounded chunk at a time,
 * so the terminal's own buffer never grows past a chunk; a resize or
 * reset runs only after everything written before it has been parsed, and a
 * reset discards whatever earlier output had not yet been handed over, so
 * nothing from a superseded screen can land after it.
 */
export class RenderQueue {
  private readonly maxChunk: number
  private readonly onIdleChange?: (idle: boolean) => void
  private ops: Op[] = []
  private inFlight = false
  private pumping = false
  private disposed = false
  private wasIdle = true
  private waiters: (() => void)[] = []

  constructor(
    private readonly terminal: WritableTerminal,
    options: RenderQueueOptions = {},
  ) {
    this.maxChunk = options.maxChunkBytes ?? 64 * 1024
    this.onIdleChange = options.onIdleChange
  }

  get isIdle() {
    return this.ops.length === 0 && !this.inFlight
  }

  write(data: Uint8Array) {
    if (this.disposed || data.length === 0) return
    for (let offset = 0; offset < data.length; offset += this.maxChunk) {
      const part = offset === 0 && data.length <= this.maxChunk ? data : data.subarray(offset, offset + this.maxChunk)
      const last = this.ops.at(-1)
      if (last?.kind === 'write' && last.size + part.length <= this.maxChunk) {
        last.parts.push(part)
        last.size += part.length
      } else {
        this.ops.push({ kind: 'write', parts: [part], size: part.length })
      }
    }
    this.pump()
  }

  resize(cols: number, rows: number) {
    if (this.disposed) return
    const last = this.ops.at(-1)
    if (last?.kind === 'resize') this.ops.pop()
    if (last?.kind === 'reset') {
      last.cols = cols
      last.rows = rows
    } else {
      this.ops.push({ kind: 'resize', cols, rows })
    }
    this.pump()
  }

  /** Starts a new screen of the given size, discarding unsent earlier work. */
  reset(cols: number, rows: number) {
    if (this.disposed) return
    this.ops = [{ kind: 'reset', cols, rows }]
    this.pump()
  }

  /** Resolves once everything queued so far has been parsed. */
  idle(): Promise<void> {
    if (this.isIdle || this.disposed) return Promise.resolve()
    return new Promise((resolve) => this.waiters.push(resolve))
  }

  dispose() {
    this.disposed = true
    this.ops = []
    this.settle()
  }

  private pump() {
    if (this.pumping || this.disposed) return
    this.pumping = true
    try {
      while (this.ops.length > 0 && !this.inFlight) {
        const op = this.ops.shift()!
        if (op.kind === 'write') {
          this.submit(op.parts.length === 1 ? op.parts[0] : concat(op.parts, op.size))
          continue
        }
        if (op.kind === 'reset') this.terminal.reset()
        this.terminal.resize(op.cols, op.rows)
      }
    } finally {
      this.pumping = false
    }
    this.settle()
  }

  private submit(chunk: Uint8Array) {
    this.inFlight = true
    this.settle()
    this.terminal.write(chunk, () => {
      if (this.disposed) return
      this.inFlight = false
      this.pump()
    })
  }

  private settle() {
    const idle = this.isIdle || this.disposed
    if (idle !== this.wasIdle) {
      this.wasIdle = idle
      this.onIdleChange?.(idle)
    }
    if (idle && this.waiters.length > 0) {
      const waiters = this.waiters
      this.waiters = []
      waiters.forEach((resolve) => resolve())
    }
  }
}

function concat(parts: Uint8Array[], size: number) {
  const out = new Uint8Array(size)
  let offset = 0
  for (const part of parts) {
    out.set(part, offset)
    offset += part.length
  }
  return out
}
