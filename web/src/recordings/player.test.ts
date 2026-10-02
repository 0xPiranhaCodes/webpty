import { describe, expect, test, vi } from 'vitest'

import type { PlaybackEvent } from '../api/types'
import { AsyncTerminal } from '../test/asyncTerminal'
import { RenderQueue } from '../terminal/renderQueue'
import {
  loadAllEvents,
  Player,
  type PlayerClock,
  type TerminalSink,
} from './player'

const enc = (s: string) => new TextEncoder().encode(s)

const events: PlaybackEvent[] = [
  { seq: 1, offsetMs: 0, kind: 'lifecycle', state: 'running' },
  { seq: 2, offsetMs: 0, kind: 'output', data: enc('$ ') },
  { seq: 3, offsetMs: 1200, kind: 'output', data: enc('ls\r\n') },
  {
    seq: 4,
    offsetMs: 1500,
    kind: 'presence',
    event: 'joined',
    participantId: 'pt_1',
    role: 'viewer',
  },
  { seq: 5, offsetMs: 2000, kind: 'resize', rows: 30, cols: 100 },
  { seq: 6, offsetMs: 2500, kind: 'output', data: enc('README.md\r\n') },
  { seq: 7, offsetMs: 4000, kind: 'lifecycle', state: 'exited', exitCode: 0 },
]

/** Models what a terminal would show: text since the last reset and its size. */
class Screen implements TerminalSink {
  text = ''
  size = ''
  resets = 0
  reset(cols: number, rows: number) {
    this.text = ''
    this.size = `${cols}x${rows}`
    this.resets++
  }
  resize(cols: number, rows: number) {
    this.size = `${cols}x${rows}`
  }
  write(data: Uint8Array) {
    this.text += new TextDecoder().decode(data)
  }
}

class FakeClock implements PlayerClock {
  time = 0
  private callbacks = new Set<() => void>()
  now() {
    return this.time
  }
  schedule(callback: () => void) {
    this.callbacks.add(callback)
    return () => this.callbacks.delete(callback)
  }
  advance(ms: number, step = 16) {
    for (let elapsed = 0; elapsed < ms; ) {
      const d = Math.min(step, ms - elapsed)
      this.time += d
      elapsed += d
      const pending = [...this.callbacks]
      this.callbacks.clear()
      pending.forEach((cb) => cb())
    }
  }
}

function setup(durationMs = 4000) {
  const screen = new Screen()
  const clock = new FakeClock()
  const onChange = vi.fn()
  const player = new Player({
    events,
    durationMs,
    rows: 24,
    cols: 80,
    sink: screen,
    clock,
    onChange,
  })
  return { screen, clock, player, onChange }
}

describe('playback', () => {
  test('starts at the first frame with the recorded size', () => {
    const { screen, player } = setup()
    expect(screen.size).toBe('80x24')
    expect(screen.text).toBe('$ ')
    expect(player.positionMs).toBe(0)
    expect(player.playing).toBe(false)
  })

  test('plays in real time and stops at the end', () => {
    const { screen, clock, player } = setup()
    player.play()
    clock.advance(1199)
    expect(screen.text).toBe('$ ')
    clock.advance(1)
    expect(screen.text).toBe('$ ls\r\n')
    clock.advance(800)
    expect(screen.size).toBe('100x30')
    clock.advance(2500)
    expect(screen.text).toBe('$ ls\r\nREADME.md\r\n')
    expect(player.playing).toBe(false)
    expect(player.positionMs).toBe(4000)
  })

  test.each([
    [0.5, 2400, '$ ls\r\n'],
    [1.5, 1000, '$ ls\r\n'],
    [2, 1250, '$ ls\r\nREADME.md\r\n'],
  ])(
    'speed %sx covers recording time proportionally',
    (speed, wallMs, shown) => {
      const { screen, clock, player } = setup()
      player.setSpeed(speed as 0.5 | 1.5 | 2)
      player.play()
      clock.advance(wallMs)
      expect(screen.text).toBe(shown)
      expect(player.positionMs).toBeCloseTo(wallMs * speed, 0)
    },
  )

  test('changing speed mid-play keeps the position continuous', () => {
    const { clock, player } = setup()
    player.play()
    clock.advance(1000)
    player.setSpeed(2)
    expect(player.positionMs).toBe(1000)
    clock.advance(500)
    expect(player.positionMs).toBe(2000)
  })

  test('pause freezes the position', () => {
    const { clock, player, screen } = setup()
    player.play()
    clock.advance(1300)
    player.pause()
    clock.advance(5000)
    expect(player.positionMs).toBe(1300)
    expect(screen.text).toBe('$ ls\r\n')
  })

  test('seeking is deterministic regardless of direction or history', () => {
    const a = setup()
    a.player.seek(3000)
    a.player.seek(1000)
    const b = setup()
    b.player.play()
    b.clock.advance(3500)
    b.player.seek(1000)
    const c = setup()
    c.player.seek(1000)

    for (const s of [a.screen, b.screen, c.screen]) {
      expect(s.text).toBe('$ ')
      expect(s.size).toBe('80x24')
    }
    a.player.seek(2600)
    expect(a.screen.text).toBe('$ ls\r\nREADME.md\r\n')
    expect(a.screen.size).toBe('100x30')
  })

  test('seek clamps to the recording and play at the end restarts', () => {
    const { player, clock, screen } = setup()
    player.seek(99_999)
    expect(player.positionMs).toBe(4000)
    player.seek(-5)
    expect(player.positionMs).toBe(0)
    player.seek(4000)
    player.play()
    expect(player.positionMs).toBe(0)
    clock.advance(1200)
    expect(screen.text).toBe('$ ls\r\n')
  })

  test('seeking while playing continues from the new position', () => {
    const { player, clock } = setup()
    player.play()
    clock.advance(500)
    player.seek(2000)
    clock.advance(100)
    expect(player.positionMs).toBe(2100)
    expect(player.playing).toBe(true)
  })

  test('an incomplete recording uses its last event when the duration is short', () => {
    const { player } = setup(0)
    expect(player.durationMs).toBe(4000)
  })

  test('requested seeks run at most once per frame and only for the latest target', () => {
    const { player, clock, screen, onChange } = setup()
    const before = screen.resets
    onChange.mockClear()
    for (const ms of [100, 3000, 1300, 2600]) player.requestSeek(ms)
    expect(screen.resets).toBe(before)
    expect(player.positionMs).toBe(2600)
    expect(onChange).toHaveBeenCalled()
    clock.advance(16)
    expect(screen.resets).toBe(before + 1)
    expect(screen.text).toBe('$ ls\r\nREADME.md\r\n')
    clock.advance(64)
    expect(screen.resets).toBe(before + 1)
  })

  test('an immediate seek supersedes a pending requested one', () => {
    const { player, clock, screen } = setup()
    player.requestSeek(3000)
    player.seek(1300)
    clock.advance(32)
    expect(player.positionMs).toBe(1300)
    expect(screen.text).toBe('$ ls\r\n')
  })

  test('dispose cancels a pending requested seek', () => {
    const { player, clock, screen } = setup()
    const before = screen.resets
    player.requestSeek(3000)
    player.dispose()
    clock.advance(32)
    expect(screen.resets).toBe(before)
  })

  test('through an asynchronous terminal, rapid seeks end on exactly the target screen', async () => {
    const terminal = new AsyncTerminal()
    const queue = new RenderQueue(terminal, { maxChunkBytes: 4 })
    const player = new Player({
      events,
      durationMs: 4000,
      rows: 24,
      cols: 80,
      sink: queue,
      clock: new FakeClock(),
    })
    for (let i = 0; i < 30; i++) {
      player.seek(4000)
      player.seek(0)
    }
    player.seek(4000)
    await queue.idle()
    expect(terminal.text).toBe('$ ls\r\nREADME.md\r\n')
    expect(`${terminal.cols}x${terminal.rows}`).toBe('100x30')
    // README was parsed after the recorded resize, never before it.
    expect(terminal.parsedAt.at(-1)).toBe('100x30')
    player.seek(1300)
    await queue.idle()
    expect(terminal.text).toBe('$ ls\r\n')
    expect(`${terminal.cols}x${terminal.rows}`).toBe('80x24')
  })

  test('notifies listeners about state changes', () => {
    const { player, onChange } = setup()
    onChange.mockClear()
    player.play()
    player.pause()
    player.setSpeed(0.5)
    expect(onChange).toHaveBeenCalledTimes(3)
  })
})

describe('loadAllEvents', () => {
  test('follows cursors until the recording is exhausted', async () => {
    const pages = [
      { events: events.slice(0, 3), next: { afterMs: 1200, afterSeq: 3 } },
      { events: events.slice(3), next: null },
    ]
    const cursors: unknown[] = []
    const progress: number[] = []
    const all = await loadAllEvents(
      async (cursor) => {
        cursors.push(cursor)
        return pages[cursors.length - 1]
      },
      (n) => progress.push(n),
    )
    expect(all.map((e) => e.seq)).toEqual([1, 2, 3, 4, 5, 6, 7])
    expect(cursors).toEqual([
      { afterMs: 0, afterSeq: 0 },
      { afterMs: 1200, afterSeq: 3 },
    ])
    expect(progress).toEqual([3, 7])
  })

  test('rejects a cursor that does not advance', async () => {
    await expect(
      loadAllEvents(async () => ({
        events: [],
        next: { afterMs: 0, afterSeq: 0 },
      })),
    ).rejects.toThrow(/did not advance/)
  })
})
