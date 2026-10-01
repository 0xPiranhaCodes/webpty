import type { PlaybackCursor, PlaybackEvent } from '../api/types'

/**
 * Where playback draws. Calls are ordered: a resize applies after the writes
 * before it, and a reset starts a new screen that nothing earlier may reach.
 */
export interface TerminalSink {
  reset(cols: number, rows: number): void
  resize(cols: number, rows: number): void
  write(data: Uint8Array): void
}

export interface PlayerClock {
  now(): number
  /** Runs callback on the next frame; returns a cancel function. */
  schedule(callback: () => void): () => void
}

export const speeds = [0.5, 1, 1.5, 2] as const
export type Speed = (typeof speeds)[number]

export const animationClock: PlayerClock = {
  now: () => performance.now(),
  schedule: (callback) => {
    const id = requestAnimationFrame(() => callback())
    return () => cancelAnimationFrame(id)
  },
}

export interface PlayerOptions {
  events: PlaybackEvent[]
  durationMs: number
  rows: number
  cols: number
  sink: TerminalSink
  clock?: PlayerClock
  onChange?: () => void
}

/**
 * Replays recorded output into a terminal. Seeking always resets the
 * terminal and re-applies every event up to the target, so the screen at a
 * given position does not depend on how playback got there.
 */
export class Player {
  readonly durationMs: number
  private readonly events: PlaybackEvent[]
  private readonly sink: TerminalSink
  private readonly clock: PlayerClock
  private readonly onChange?: () => void
  private readonly rows: number
  private readonly cols: number
  private index = 0
  private position = 0
  private anchorPosition = 0
  private anchorTime = 0
  private cancelFrame: (() => void) | null = null
  private currentSpeed: Speed = 1
  private requestedMs: number | null = null
  private cancelSeek: (() => void) | null = null

  constructor(options: PlayerOptions) {
    this.events = options.events
    this.sink = options.sink
    this.clock = options.clock ?? animationClock
    this.onChange = options.onChange
    this.rows = options.rows
    this.cols = options.cols
    const last = this.events.at(-1)?.offsetMs ?? 0
    this.durationMs = Math.max(options.durationMs, last)
    this.render(0)
  }

  /** The position shown to the user, including a seek that has not run yet. */
  get positionMs() {
    return this.requestedMs ?? this.position
  }

  get playing() {
    return this.cancelFrame !== null
  }

  get speed() {
    return this.currentSpeed
  }

  play() {
    if (this.playing) return
    if (this.position >= this.durationMs) this.render(0)
    this.anchor()
    this.cancelFrame = this.clock.schedule(this.tick)
    this.onChange?.()
  }

  pause() {
    if (!this.playing) return
    this.advanceTo(this.currentPosition())
    this.stopFrames()
    this.onChange?.()
  }

  toggle() {
    if (this.playing) this.pause()
    else this.play()
  }

  setSpeed(speed: Speed) {
    if (this.playing) {
      this.advanceTo(this.currentPosition())
      this.currentSpeed = speed
      this.anchor()
    } else {
      this.currentSpeed = speed
    }
    this.onChange?.()
  }

  /**
   * Seeks on the next frame. Requests made before then collapse into the
   * latest one, so dragging the slider redraws at most once per frame.
   */
  requestSeek(ms: number) {
    this.requestedMs = this.clamp(ms)
    if (!this.cancelSeek) {
      this.cancelSeek = this.clock.schedule(() => {
        this.cancelSeek = null
        if (this.requestedMs !== null) this.seek(this.requestedMs)
      })
    }
    this.onChange?.()
  }

  seek(ms: number) {
    this.clearRequestedSeek()
    this.render(this.clamp(ms))
    if (this.playing) this.anchor()
    this.onChange?.()
  }

  dispose() {
    this.stopFrames()
    this.clearRequestedSeek()
  }

  private clamp(ms: number) {
    return Math.min(Math.max(0, Math.round(ms)), this.durationMs)
  }

  private clearRequestedSeek() {
    this.cancelSeek?.()
    this.cancelSeek = null
    this.requestedMs = null
  }

  private tick = () => {
    this.cancelFrame = null
    const target = this.currentPosition()
    this.advanceTo(target)
    if (target >= this.durationMs) {
      this.onChange?.()
      return
    }
    this.cancelFrame = this.clock.schedule(this.tick)
    this.onChange?.()
  }

  private currentPosition() {
    const elapsed = (this.clock.now() - this.anchorTime) * this.currentSpeed
    return Math.min(this.durationMs, Math.round(this.anchorPosition + elapsed))
  }

  private anchor() {
    this.anchorPosition = this.position
    this.anchorTime = this.clock.now()
  }

  private stopFrames() {
    this.cancelFrame?.()
    this.cancelFrame = null
  }

  private render(ms: number) {
    this.sink.reset(this.cols, this.rows)
    this.index = 0
    this.position = 0
    this.advanceTo(ms)
  }

  private advanceTo(ms: number) {
    while (this.index < this.events.length && this.events[this.index].offsetMs <= ms) {
      const event = this.events[this.index++]
      if (event.kind === 'output') this.sink.write(event.data)
      else if (event.kind === 'resize') this.sink.resize(event.cols, event.rows)
    }
    this.position = ms
  }
}

export type PageFetcher = (cursor: PlaybackCursor) => Promise<{ events: PlaybackEvent[]; next: PlaybackCursor | null }>

/** Loads every page of a recording, following the server's cursors. */
export async function loadAllEvents(fetchPage: PageFetcher, onProgress?: (loaded: number) => void): Promise<PlaybackEvent[]> {
  const all: PlaybackEvent[] = []
  let cursor: PlaybackCursor = { afterMs: 0, afterSeq: 0 }
  for (;;) {
    const page = await fetchPage(cursor)
    all.push(...page.events)
    onProgress?.(all.length)
    if (!page.next) return all
    if (page.next.afterSeq <= cursor.afterSeq && page.next.afterMs <= cursor.afterMs) {
      throw new Error('recording cursor did not advance')
    }
    cursor = page.next
  }
}
