import { arrayOf, type Decoded, DecodeError, nullable, num, object, oneOf, optional, str } from '../api/decode'
import { base64ToBytes, decodePermissions, terminalStates } from '../api/types'

/** WebSocket subprotocol spoken by internal/httpapi/terminal_ws.go. */
export const terminalProtocol = 'webpty.terminal.v1'

/** Application close codes. */
export const CloseCode = {
  Normal: 1000,
  PolicyViolation: 1008,
  SlowConsumer: 4000,
  ReplayGap: 4001,
  Unauthorized: 4003,
  InputOverflow: 4004,
} as const

const role = oneOf('owner', 'editor', 'viewer')
const participant = object({ id: str, role })
export type Participant = Decoded<typeof participant>

const readySession = object({ id: str, state: oneOf(...terminalStates), rows: num, cols: num })

const decoders = {
  ready: object({ version: num, session: readySession, seq: num, role, permissions: decodePermissions }),
  output: object({ seq: num, data: str }),
  exit: object({ state: str, exitCode: nullable(num), signal: optional(str) }),
  error: object({
    code: str,
    message: str,
    action: optional(str),
    firstSeq: optional(num),
    lastSeq: optional(num),
  }),
  pong: object({}),
  resize: object({ rows: num, cols: num }),
  presence_snapshot: object({ version: num, self: str, participants: arrayOf(participant) }),
  participant_joined: object({ version: num, participant }),
  participant_left: object({ version: num, participant, reason: optional(str) }),
  permission_changed: object({ version: num, participant, permissions: decodePermissions, reason: optional(str) }),
  recording_status: object({ recordingId: str, status: str, code: str, message: str }),
}

type Raw = { [K in keyof typeof decoders]: { type: K } & Decoded<(typeof decoders)[K]> }
export type ReadyMessage = Raw['ready']
export type ExitMessage = Raw['exit']
export type ErrorMessage = Raw['error']
export type RecordingStatusMessage = Raw['recording_status']
export type PresenceMessage = Raw['presence_snapshot' | 'participant_joined' | 'participant_left' | 'permission_changed']
export type OutputMessage = { type: 'output'; seq: number; data: Uint8Array }
export type ServerMessage = Exclude<Raw[keyof Raw], Raw['output']> | OutputMessage

const validSize = (n: number) => Number.isInteger(n) && n > 0 && n <= 1000

/** Parses one server frame; malformed or unknown frames yield null. */
export function parseServerMessage(raw: string): ServerMessage | null {
  let value: unknown
  try {
    value = JSON.parse(raw)
  } catch {
    return null
  }
  if (typeof value !== 'object' || value === null) return null
  const type = (value as { type?: unknown }).type
  if (typeof type !== 'string' || !(type in decoders)) return null
  try {
    const decoded = decoders[type as keyof typeof decoders](value)
    if (type === 'resize') {
      const { rows, cols } = decoded as Decoded<typeof decoders.resize>
      if (!validSize(rows) || !validSize(cols)) return null
    }
    if (type === 'output') {
      const output = decoded as Decoded<typeof decoders.output>
      return { type, seq: output.seq, data: base64ToBytes(output.data) }
    }
    return { type, ...decoded } as ServerMessage
  } catch (error) {
    if (error instanceof DecodeError || error instanceof DOMException) return null
    throw error
  }
}

export const encodeInput = (data: string) => JSON.stringify({ type: 'input', data })
export const encodeResize = (rows: number, cols: number) => JSON.stringify({ type: 'resize', rows, cols })
export const encodePing = () => JSON.stringify({ type: 'ping' })
