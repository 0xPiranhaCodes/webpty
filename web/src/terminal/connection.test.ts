import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import {
  type ConnectionState,
  TerminalConnection,
  type WebSocketLike,
} from './connection'
import { parseServerMessage } from './protocol'

class FakeSocket implements WebSocketLike {
  static all: FakeSocket[] = []
  readyState = 0
  sent: string[] = []
  closed?: { code?: number; reason?: string }
  onopen: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onclose: ((event: { code: number; reason: string }) => void) | null = null
  onerror: (() => void) | null = null

  constructor(
    readonly url: string,
    readonly protocols: string | string[],
  ) {
    FakeSocket.all.push(this)
  }
  send(data: string) {
    this.sent.push(data)
  }
  close(code?: number, reason?: string) {
    this.closed = { code, reason }
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

const b64 = (text: string) => btoa(text)
const session = { id: 'tm_1', state: 'running', rows: 24, cols: 80 }

function ready(overrides: Record<string, unknown> = {}) {
  return {
    type: 'ready',
    version: 1,
    session,
    seq: 0,
    role: 'editor',
    permissions: { input: true, resize: true },
    ...overrides,
  }
}

function setup(
  options: {
    probe?: () => Promise<
      'retry' | { ended: 'exited' | 'unauthorized' | 'not_found' }
    >
  } = {},
) {
  const states: ConnectionState[] = []
  const output: string[] = []
  const readies: boolean[] = []
  const notices: string[] = []
  const recording: string[] = []
  const decoder = new TextDecoder()
  const connection = new TerminalConnection({
    terminalId: 'tm_1',
    origin: 'https://pty.example:8443',
    socketFactory: (url, protocols) => new FakeSocket(url, protocols),
    random: () => 0,
    probe: options.probe,
    handlers: {
      onState: (s) => states.push(s),
      onReady: (message, fresh) => readies.push(fresh),
      onOutput: (bytes) => output.push(decoder.decode(bytes)),
      onNotice: (code) => notices.push(code),
      onRecordingStatus: (m) => recording.push(m.code),
    },
  })
  return {
    connection,
    states,
    output,
    readies,
    notices,
    recording,
    socket: () => FakeSocket.all.at(-1)!,
  }
}

beforeEach(() => {
  FakeSocket.all = []
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('protocol parsing', () => {
  test('rejects malformed and unknown frames', () => {
    expect(parseServerMessage('not json')).toBeNull()
    expect(
      parseServerMessage('{"type":"output","seq":"1","data":"x"}'),
    ).toBeNull()
    expect(parseServerMessage('{"type":"telemetry"}')).toBeNull()
  })

  test('decodes output bytes and replay-gap errors', () => {
    const output = parseServerMessage(
      JSON.stringify({ type: 'output', seq: 3, data: b64('hi') }),
    )
    expect(output?.type === 'output' && Array.from(output.data)).toEqual([
      104, 105,
    ])
    expect(
      parseServerMessage(
        JSON.stringify({
          type: 'error',
          code: 'replay_gap',
          message: 'requested output is no longer buffered',
          firstSeq: 90,
          lastSeq: 120,
        }),
      ),
    ).toMatchObject({ type: 'error', code: 'replay_gap', firstSeq: 90 })
  })
})

describe('terminal connection', () => {
  test('opens the versioned protocol on the same origin and goes live on ready', () => {
    const t = setup()
    t.connection.start()

    expect(t.socket().url).toBe(
      'wss://pty.example:8443/api/v1/terminals/tm_1/ws',
    )
    expect(t.socket().protocols).toEqual(['webpty.terminal.v1'])
    expect(t.states.at(-1)).toEqual({ kind: 'connecting', attempt: 0 })

    t.socket().open()
    t.socket().server(ready())

    expect(t.states.at(-1)).toEqual({ kind: 'live' })
    expect(t.readies).toEqual([true])
    expect(t.connection.permissions).toEqual({ input: true, resize: true })
  })

  test('writes output once, skipping replayed sequences', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready({ seq: 2 }))
    t.socket().server({ type: 'output', seq: 1, data: b64('a') })
    t.socket().server({ type: 'output', seq: 2, data: b64('b') })
    t.socket().server({ type: 'output', seq: 2, data: b64('b') })
    t.socket().server({ type: 'output', seq: 3, data: b64('c') })
    expect(t.output.join('')).toBe('abc')
  })

  test('reconnects after a dropped connection and resumes after the last sequence', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    t.socket().server({ type: 'output', seq: 7, data: b64('x') })

    t.socket().serverClose(1006)

    expect(t.states.at(-1)).toMatchObject({
      kind: 'reconnecting',
      attempt: 1,
      retryInMs: 500,
    })
    expect(FakeSocket.all).toHaveLength(1)
    vi.advanceTimersByTime(500)
    expect(FakeSocket.all).toHaveLength(2)
    expect(t.socket().url).toBe(
      'wss://pty.example:8443/api/v1/terminals/tm_1/ws?afterSeq=7',
    )

    t.socket().open()
    t.socket().server(ready({ seq: 9 }))
    t.socket().server({ type: 'output', seq: 7, data: b64('dup') })
    t.socket().server({ type: 'output', seq: 8, data: b64('y') })

    expect(t.readies).toEqual([true, false])
    expect(t.output.join('')).toBe('xy')
    expect(t.states.at(-1)).toEqual({ kind: 'live' })
  })

  test('backs off exponentially up to a cap while the server is unreachable', () => {
    const t = setup()
    t.connection.start()
    const delays: number[] = []
    for (let i = 0; i < 7; i++) {
      t.socket().serverClose(1006)
      const state = t.states.at(-1)!
      delays.push(state.kind === 'reconnecting' ? state.retryInMs : -1)
      vi.advanceTimersByTime(
        state.kind === 'reconnecting' ? state.retryInMs : 0,
      )
    }
    expect(delays).toEqual([500, 1000, 2000, 4000, 8000, 10000, 10000])
  })

  test('a replay gap starts a fresh stream so the terminal can be redrawn', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    t.socket().server({ type: 'output', seq: 3, data: b64('old') })
    t.socket().serverClose(1006)
    vi.advanceTimersByTime(500)

    t.socket().open()
    t.socket().server({
      type: 'error',
      code: 'replay_gap',
      message: 'requested output is no longer buffered',
      firstSeq: 90,
      lastSeq: 120,
    })
    t.socket().serverClose(4001, 'replay gap')

    expect(t.notices).toContain('replay_gap')
    vi.advanceTimersByTime(0)
    expect(t.socket().url).toBe(
      'wss://pty.example:8443/api/v1/terminals/tm_1/ws',
    )
    t.socket().open()
    t.socket().server(ready({ seq: 120 }))
    t.socket().server({ type: 'output', seq: 95, data: b64('new') })
    expect(t.readies).toEqual([true, true])
    expect(t.output.join('')).toBe('oldnew')
  })

  test('revocation ends the connection with the reason and never reconnects', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(
      ready({ role: 'viewer', permissions: { input: false, resize: false } }),
    )
    t.socket().server({
      type: 'presence_snapshot',
      version: 1,
      self: 'pt_me',
      participants: [{ id: 'pt_me', role: 'viewer' }],
    })
    t.socket().server({
      type: 'permission_changed',
      version: 2,
      participant: { id: 'pt_me', role: 'viewer' },
      permissions: { input: false, resize: false },
      reason: 'revoked',
    })
    t.socket().serverClose(4003, 'access revoked')
    vi.advanceTimersByTime(60_000)

    expect(t.states.at(-1)).toEqual({ kind: 'ended', reason: 'revoked' })
    expect(FakeSocket.all).toHaveLength(1)
  })

  test('an expired admin session ends as unauthorized', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready({ role: 'owner' }))
    t.socket().serverClose(4003, 'session expired')
    expect(t.states.at(-1)).toEqual({ kind: 'ended', reason: 'unauthorized' })
  })

  test('process exit ends the stream without reconnecting', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    t.socket().server({ type: 'exit', state: 'exited', exitCode: 2 })
    t.socket().serverClose(1000, 'session ended')
    vi.advanceTimersByTime(60_000)
    expect(t.states.at(-1)).toEqual({
      kind: 'ended',
      reason: 'exited',
      exit: { state: 'exited', exitCode: 2 },
    })
    expect(FakeSocket.all).toHaveLength(1)
  })

  test('viewers cannot send input or resize; editors can', () => {
    const viewer = setup()
    viewer.connection.start()
    viewer.socket().open()
    viewer
      .socket()
      .server(
        ready({ role: 'viewer', permissions: { input: false, resize: false } }),
      )
    expect(viewer.connection.sendInput('rm -rf /\r')).toBe(false)
    expect(viewer.connection.sendResize(40, 100)).toBe(false)
    expect(viewer.socket().sent).toEqual([])

    FakeSocket.all = []
    const editor = setup()
    editor.connection.start()
    editor.socket().open()
    editor.socket().server(ready())
    expect(editor.connection.sendInput('ls\r')).toBe(true)
    expect(editor.connection.sendResize(40, 100)).toBe(true)
    expect(editor.socket().sent).toEqual([
      '{"type":"input","data":"ls\\r"}',
      '{"type":"resize","rows":40,"cols":100}',
    ])
  })

  test('losing input permission mid-session stops input immediately', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    t.socket().server({
      type: 'presence_snapshot',
      version: 1,
      self: 'pt_me',
      participants: [{ id: 'pt_me', role: 'editor' }],
    })
    t.socket().server({
      type: 'permission_changed',
      version: 2,
      participant: { id: 'pt_other', role: 'viewer' },
      permissions: { input: false, resize: false },
    })
    expect(t.connection.permissions.input).toBe(true)
    t.socket().server({
      type: 'permission_changed',
      version: 3,
      participant: { id: 'pt_me', role: 'editor' },
      permissions: { input: false, resize: false },
      reason: 'replaced',
    })
    expect(t.connection.sendInput('x')).toBe(false)
  })

  test('process exit withdraws input before the socket closes', () => {
    const permissions: boolean[] = []
    const connection = new TerminalConnection({
      terminalId: 'tm_1',
      origin: 'https://pty.example:8443',
      socketFactory: (url, protocols) => new FakeSocket(url, protocols),
      handlers: { onPermissions: (p) => permissions.push(p.input) },
    })
    connection.start()
    FakeSocket.all.at(-1)!.open()
    FakeSocket.all.at(-1)!.server(ready())
    FakeSocket.all
      .at(-1)!
      .server({ type: 'exit', state: 'exited', exitCode: 0 })
    expect(permissions).toEqual([true, false])
    expect(connection.sendInput('x')).toBe(false)
  })

  test('the owner’s screen size is reported to everyone else', () => {
    const sizes: [number, number][] = []
    const connection = new TerminalConnection({
      terminalId: 'tm_1',
      origin: 'https://pty.example:8443',
      socketFactory: (url, protocols) => new FakeSocket(url, protocols),
      handlers: { onResize: (rows, cols) => sizes.push([rows, cols]) },
    })
    connection.start()
    FakeSocket.all.at(-1)!.open()
    FakeSocket.all.at(-1)!.server(ready())
    FakeSocket.all.at(-1)!.server({ type: 'resize', rows: 40, cols: 100 })
    FakeSocket.all.at(-1)!.server({ type: 'resize', rows: -1, cols: 100 })
    expect(sizes).toEqual([[40, 100]])
  })

  test('access superseded by a newer link in this browser ends with that reason', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    t.socket().serverClose(4003, 'access superseded')
    expect(t.states.at(-1)).toEqual({ kind: 'ended', reason: 'superseded' })
  })

  test('input overflow is reported and the stream resumes', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    t.socket().server({ type: 'output', seq: 4, data: b64('z') })
    t.socket().server({
      type: 'error',
      code: 'input_overflow',
      message: 'terminal is not accepting input fast enough',
    })
    t.socket().serverClose(4004, 'input backlog')
    expect(t.notices).toContain('input_overflow')
    vi.advanceTimersByTime(500)
    expect(t.socket().url).toContain('afterSeq=4')
  })

  test('a handshake that never opens is checked before retrying', async () => {
    const probe = vi.fn(async () => ({ ended: 'exited' as const }))
    const t = setup({ probe })
    t.connection.start()
    t.socket().serverClose(1006)
    await vi.runAllTimersAsync()
    expect(probe).toHaveBeenCalledTimes(1)
    expect(t.states.at(-1)).toEqual({ kind: 'ended', reason: 'exited' })
    expect(FakeSocket.all).toHaveLength(1)
  })

  test('recording warnings are forwarded', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready({ role: 'owner' }))
    t.socket().server({
      type: 'recording_status',
      recordingId: 'rc_1',
      status: 'incomplete',
      code: 'queue_overflow',
      message:
        'recording stopped: output arrived faster than it could be stored',
    })
    expect(t.recording).toEqual(['queue_overflow'])
  })

  test('stop closes the socket and cancels reconnects', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    t.socket().serverClose(1006)
    t.connection.stop()
    vi.advanceTimersByTime(60_000)
    expect(FakeSocket.all).toHaveLength(1)
  })

  test('a silent connection is detected by the keepalive and replaced', () => {
    const t = setup()
    t.connection.start()
    t.socket().open()
    t.socket().server(ready())
    vi.advanceTimersByTime(25_000)
    expect(t.socket().sent).toContain('{"type":"ping"}')
    vi.advanceTimersByTime(10_000)
    expect(t.socket().closed).toBeDefined()
  })
})
