import { describe, expect, test, vi } from 'vitest'

import { AsyncTerminal } from '../test/asyncTerminal'
import { RenderQueue } from './renderQueue'

const enc = (s: string) => new TextEncoder().encode(s)

function manual(maxChunkBytes = 1024) {
  const terminal = new AsyncTerminal({ autoDrain: false })
  const queue = new RenderQueue(terminal, { maxChunkBytes })
  return { terminal, queue }
}

describe('RenderQueue', () => {
  test('coalesces consecutive output into one write', () => {
    const { terminal, queue } = manual()
    queue.write(enc('a'))
    queue.write(enc('b'))
    queue.write(enc('c'))
    queue.write(enc('d'))
    expect(terminal.writes).toBe(1)
    terminal.drainAll()
    expect(terminal.text).toBe('abcd')
    expect(terminal.writes).toBe(2)
  })

  test('splits large output so the terminal never holds more than one chunk', () => {
    const { terminal, queue } = manual(100)
    queue.write(new Uint8Array(1000).fill(120))
    expect(terminal.queued).toBe(1)
    expect(terminal.maxPendingBytes).toBe(100)
    terminal.drainAll()
    expect(terminal.text).toHaveLength(1000)
    expect(terminal.maxPendingBytes).toBe(100)
  })

  test('applies a resize only after earlier output has been parsed', () => {
    const { terminal, queue } = manual()
    queue.write(enc('wide'))
    queue.resize(40, 10)
    queue.write(enc('narrow'))
    expect(`${terminal.cols}x${terminal.rows}`).toBe('80x24')
    terminal.drainOne()
    expect(`${terminal.cols}x${terminal.rows}`).toBe('40x10')
    terminal.drainOne()
    expect(terminal.text).toBe('widenarrow')
    expect(terminal.parsedAt).toEqual(['80x24', '40x10'])
  })

  test('reset waits for in-flight output, drops what was not sent, and starts clean', () => {
    const { terminal, queue } = manual()
    queue.write(enc('old-1'))
    queue.write(enc('old-2'))
    queue.resize(10, 5)
    queue.reset(100, 30)
    queue.write(enc('new'))
    // old-1 was already handed to the terminal; nothing may follow it until it is parsed.
    expect(terminal.queued).toBe(1)
    terminal.drainAll()
    terminal.drainAll()
    expect(terminal.text).toBe('new')
    expect(`${terminal.cols}x${terminal.rows}`).toBe('100x30')
  })

  test('repeated resets keep only the newest generation', async () => {
    const terminal = new AsyncTerminal()
    const queue = new RenderQueue(terminal, { maxChunkBytes: 8 })
    for (let generation = 0; generation < 50; generation++) {
      queue.reset(80, 24)
      for (let line = 0; line < 20; line++)
        queue.write(enc(`g${generation}:${line}\n`))
    }
    await queue.idle()
    expect(terminal.text).toBe(
      Array.from({ length: 20 }, (_, line) => `g49:${line}\n`).join(''),
    )
  })

  test('megabytes of queued output never exceed the terminal watermark', async () => {
    const terminal = new AsyncTerminal({ watermark: 300_000 })
    const queue = new RenderQueue(terminal)
    const line = enc('x'.repeat(1023) + '\n')
    for (let i = 0; i < 8 * 1024; i++) queue.write(line)
    await queue.idle()
    expect(terminal.text).toHaveLength(8 * 1024 * 1024)
    expect(terminal.maxPendingBytes).toBe(64 * 1024)
  })

  test('reports when it is busy and when it is idle', async () => {
    const onIdleChange = vi.fn()
    const terminal = new AsyncTerminal()
    const queue = new RenderQueue(terminal, { onIdleChange })
    expect(queue.isIdle).toBe(true)
    queue.write(enc('x'))
    expect(queue.isIdle).toBe(false)
    await queue.idle()
    expect(queue.isIdle).toBe(true)
    expect(onIdleChange.mock.calls).toEqual([[false], [true]])
  })

  test('ignores callbacks and writes after dispose', () => {
    const { terminal, queue } = manual()
    queue.write(enc('a'))
    queue.dispose()
    queue.write(enc('b'))
    terminal.drainAll()
    terminal.drainAll()
    expect(terminal.text).toBe('a')
  })
})
