import type { Permissions } from '../api/types'
import {
  CloseCode,
  encodeInput,
  encodePing,
  encodeResize,
  type ErrorMessage,
  type ExitMessage,
  parseServerMessage,
  type Participant,
  type ReadyMessage,
  type RecordingStatusMessage,
  type ServerMessage,
  terminalProtocol,
} from './protocol'

export type EndReason =
  | 'exited'
  | 'unauthorized'
  | 'revoked'
  | 'replaced'
  | 'superseded'
  | 'expired'
  | 'logged_out'
  | 'not_found'
  | 'closed'

export interface ExitStatus {
  state: string
  exitCode: number | null
  signal?: string
}

export type ConnectionState =
  | { kind: 'connecting'; attempt: number }
  | { kind: 'live' }
  | { kind: 'reconnecting'; attempt: number; retryInMs: number }
  | { kind: 'ended'; reason: EndReason; exit?: ExitStatus }

export interface WebSocketLike {
  readonly readyState: number
  send(data: string): void
  close(code?: number, reason?: string): void
  onopen: (() => void) | null
  onmessage: ((event: { data: unknown }) => void) | null
  onclose: ((event: { code: number; reason: string }) => void) | null
  onerror: (() => void) | null
}

export type ProbeResult = 'retry' | { ended: EndReason }

export interface ConnectionHandlers {
  onState?(state: ConnectionState): void
  /** fresh is true when the stream restarted from the replay buffer and the terminal must be cleared first. */
  onReady?(message: ReadyMessage, fresh: boolean): void
  onOutput?(data: Uint8Array, seq: number): void
  onExit?(message: ExitMessage): void
  onPresence?(participants: Participant[], selfId: string | null): void
  onPermissions?(permissions: Permissions): void
  /** The terminal's size changed; anyone who may not resize should show this size. */
  onResize?(rows: number, cols: number): void
  onRecordingStatus?(message: RecordingStatusMessage): void
  onNotice?(code: string, message: ErrorMessage): void
}

export interface ConnectionOptions {
  terminalId: string
  handlers: ConnectionHandlers
  origin?: string
  socketFactory?: (url: string, protocols: string[]) => WebSocketLike
  /** Called when a handshake fails before opening, to tell a dead session from a network fault. */
  probe?: () => Promise<ProbeResult>
  random?: () => number
}

const baseDelayMs = 500
const maxDelayMs = 10_000
const pingIntervalMs = 25_000
const pongTimeoutMs = 10_000

const closeReasons: Record<string, EndReason> = {
  'access revoked': 'revoked',
  'access replaced': 'replaced',
  'access superseded': 'superseded',
  'access expired': 'expired',
  'access logout': 'logged_out',
}

const noPermissions: Permissions = { input: false, resize: false }

export class TerminalConnection {
  private socket: WebSocketLike | null = null
  private opened = false
  private attempt = 0
  private lastSeq: number | null = null
  private exit: ExitMessage | null = null
  private stopped = true
  private retryTimer: ReturnType<typeof setTimeout> | undefined
  private pingTimer: ReturnType<typeof setInterval> | undefined
  private pongTimer: ReturnType<typeof setTimeout> | undefined
  private selfId: string | null = null
  private presenceVersion = -1
  private participants: Participant[] = []
  private revokedReason: string | undefined
  private currentPermissions: Permissions = noPermissions
  private currentState: ConnectionState = { kind: 'connecting', attempt: 0 }

  constructor(private readonly options: ConnectionOptions) {}

  get permissions(): Permissions {
    return this.currentPermissions
  }

  get state(): ConnectionState {
    return this.currentState
  }

  start() {
    if (!this.stopped) return
    this.stopped = false
    this.connect()
  }

  stop() {
    this.stopped = true
    clearTimeout(this.retryTimer)
    this.detach(CloseCode.Normal, 'client closed')
  }

  sendInput(data: string): boolean {
    if (!this.currentPermissions.input || !this.isLive() || data === '') return false
    this.socket!.send(encodeInput(data))
    return true
  }

  sendResize(rows: number, cols: number): boolean {
    if (!this.currentPermissions.resize || !this.isLive()) return false
    this.socket!.send(encodeResize(rows, cols))
    return true
  }

  private isLive() {
    return this.currentState.kind === 'live' && this.socket?.readyState === 1
  }

  private setState(state: ConnectionState) {
    this.currentState = state
    this.options.handlers.onState?.(state)
  }

  private url() {
    const origin = this.options.origin ?? window.location.origin
    const base = origin.replace(/^http/, 'ws') + `/api/v1/terminals/${encodeURIComponent(this.options.terminalId)}/ws`
    return this.lastSeq === null ? base : `${base}?afterSeq=${this.lastSeq}`
  }

  private connect() {
    this.setState({ kind: 'connecting', attempt: this.attempt })
    const factory = this.options.socketFactory ?? ((url, protocols) => new WebSocket(url, protocols) as WebSocketLike)
    const socket = factory(this.url(), [terminalProtocol])
    this.socket = socket
    this.opened = false
    this.revokedReason = undefined
    socket.onopen = () => {
      if (this.socket !== socket) return
      this.opened = true
      this.startKeepalive()
    }
    socket.onmessage = (event) => {
      if (this.socket !== socket || typeof event.data !== 'string') return
      clearTimeout(this.pongTimer)
      const message = parseServerMessage(event.data)
      if (message) this.handle(message)
    }
    socket.onclose = (event) => {
      if (this.socket !== socket) return
      this.onClosed(event.code, event.reason)
    }
    socket.onerror = () => {}
  }

  private handle(message: ServerMessage) {
    const h = this.options.handlers
    switch (message.type) {
      case 'ready': {
        const fresh = this.lastSeq === null
        this.attempt = 0
        this.currentPermissions = message.permissions
        this.setState({ kind: 'live' })
        h.onReady?.(message, fresh)
        h.onPermissions?.(message.permissions)
        return
      }
      case 'output':
        if (this.lastSeq !== null && message.seq <= this.lastSeq) return
        this.lastSeq = message.seq
        h.onOutput?.(message.data, message.seq)
        return
      case 'exit':
        this.exit = message
        this.currentPermissions = noPermissions
        h.onPermissions?.(noPermissions)
        h.onExit?.(message)
        return
      case 'resize':
        h.onResize?.(message.rows, message.cols)
        return
      case 'error':
        if (message.code === 'replay_gap') this.lastSeq = null
        h.onNotice?.(message.code, message)
        return
      case 'recording_status':
        h.onRecordingStatus?.(message)
        return
      case 'pong':
        return
      case 'presence_snapshot':
        this.selfId = message.self
        this.presenceVersion = message.version
        this.participants = message.participants
        h.onPresence?.(this.participants, this.selfId)
        return
      default:
        this.applyPresence(message)
    }
  }

  private applyPresence(message: Exclude<ServerMessage, { type: 'presence_snapshot' }> & { version: number; participant: Participant }) {
    if (message.version <= this.presenceVersion) return
    this.presenceVersion = message.version
    const id = message.participant.id
    const h = this.options.handlers
    switch (message.type) {
      case 'participant_joined':
        this.participants = [...this.participants.filter((p) => p.id !== id), message.participant]
        break
      case 'participant_left':
        this.participants = this.participants.filter((p) => p.id !== id)
        break
      case 'permission_changed':
        if (id === this.selfId) {
          this.currentPermissions = message.permissions
          this.revokedReason = message.reason
          h.onPermissions?.(message.permissions)
        }
        if (message.reason) this.participants = this.participants.filter((p) => p.id !== id)
        break
    }
    h.onPresence?.(this.participants, this.selfId)
  }

  private onClosed(code: number, reason: string) {
    const opened = this.opened
    this.detach()
    if (this.stopped) return
    if (this.exit || (code === CloseCode.Normal && reason === 'session ended')) {
      const exit = this.exit
      this.end('exited', exit ? { state: exit.state, exitCode: exit.exitCode, ...(exit.signal ? { signal: exit.signal } : {}) } : undefined)
      return
    }
    if (code === CloseCode.Unauthorized) {
      const why = closeReasons[reason] ?? (this.revokedReason ? closeReasons[`access ${this.revokedReason}`] : undefined)
      this.end(why ?? 'unauthorized')
      return
    }
    if (code === CloseCode.PolicyViolation) {
      this.end('closed')
      return
    }
    if (code === CloseCode.ReplayGap) {
      this.lastSeq = null
      this.schedule(0)
      return
    }
    this.attempt += 1
    const delay = Math.min(baseDelayMs * 2 ** (this.attempt - 1), maxDelayMs) + Math.floor((this.options.random ?? Math.random)() * 250)
    if (!opened && this.options.probe) {
      void this.options.probe().then(
        (result) => {
          if (this.stopped) return
          if (result === 'retry') this.schedule(delay)
          else this.end(result.ended)
        },
        () => !this.stopped && this.schedule(delay),
      )
      return
    }
    this.schedule(delay)
  }

  private end(reason: EndReason, exit?: ExitStatus) {
    this.stopped = true
    this.currentPermissions = noPermissions
    this.setState(exit ? { kind: 'ended', reason, exit } : { kind: 'ended', reason })
  }

  private schedule(delay: number) {
    this.setState({ kind: 'reconnecting', attempt: this.attempt, retryInMs: delay })
    clearTimeout(this.retryTimer)
    this.retryTimer = setTimeout(() => {
      if (!this.stopped) this.connect()
    }, delay)
  }

  private startKeepalive() {
    clearInterval(this.pingTimer)
    this.pingTimer = setInterval(() => {
      if (this.socket?.readyState !== 1) return
      this.socket.send(encodePing())
      clearTimeout(this.pongTimer)
      const socket = this.socket
      this.pongTimer = setTimeout(() => {
        if (this.socket === socket) this.onClosed(1006, 'keepalive timeout')
      }, pongTimeoutMs)
    }, pingIntervalMs)
  }

  private detach(code?: number, reason?: string) {
    clearInterval(this.pingTimer)
    clearTimeout(this.pongTimer)
    const socket = this.socket
    this.socket = null
    if (!socket) return
    socket.onopen = socket.onmessage = socket.onclose = socket.onerror = null
    if (socket.readyState < 2) socket.close(code ?? 4000, reason ?? 'reconnecting')
  }
}
