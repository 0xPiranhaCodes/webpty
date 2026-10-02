import {
  arrayOf,
  bool,
  type Decoded,
  DecodeError,
  type Decoder,
  nullable,
  num,
  object,
  oneOf,
  optional,
  str,
  stringMap,
} from './decode'

export const decodeAdminSession = object({
  state: oneOf('authenticated', 'bootstrap'),
  passwordChangeRequired: bool,
  csrfToken: str,
  expiresAt: str,
})
export type AdminSession = Decoded<typeof decodeAdminSession>

export const terminalStates = [
  'starting',
  'running',
  'exited',
  'failed',
  'terminated',
] as const
export type TerminalState = (typeof terminalStates)[number]

export const decodeTerminal = object({
  id: str,
  state: oneOf(...terminalStates),
  command: str,
  args: arrayOf(str),
  rows: num,
  cols: num,
  createdAt: str,
  startedAt: nullable(str),
  endedAt: nullable(str),
  lastActivityAt: str,
  exitCode: nullable(num),
  exitSignal: optional(str),
  failure: optional(str),
  participants: num,
})
export type Terminal = Decoded<typeof decodeTerminal>

export const decodeSharedTerminal = object({
  id: str,
  state: oneOf(...terminalStates),
  rows: num,
  cols: num,
  createdAt: str,
  startedAt: nullable(str),
  endedAt: nullable(str),
  exitCode: nullable(num),
  exitSignal: optional(str),
})
export type SharedTerminal = Decoded<typeof decodeSharedTerminal>

export const decodePermissions = object({ input: bool, resize: bool })
export type Permissions = Decoded<typeof decodePermissions>

export const grantRoles = ['editor', 'viewer'] as const
export type GrantRole = (typeof grantRoles)[number]
export type ParticipantRole = 'owner' | GrantRole

export const decodeGrant = object({
  id: str,
  terminalId: str,
  role: oneOf(...grantRoles),
  label: str,
  singleUse: bool,
  maxRedemptions: num,
  redemptionCount: num,
  status: oneOf('active', 'revoked', 'replaced', 'expired', 'exhausted'),
  createdAt: str,
  updatedAt: str,
  expiresAt: str,
  redeemedAt: nullable(str),
  revokedAt: nullable(str),
  revokeReason: optional(str),
})
export type Grant = Decoded<typeof decodeGrant>

export const decodeCreatedGrant = object({
  grant: decodeGrant,
  token: str,
  inviteUrl: str,
  replaced: arrayOf(str),
})
export type CreatedGrant = Decoded<typeof decodeCreatedGrant>

export const decodeGuestSession = object({
  role: oneOf(...grantRoles),
  grantId: str,
  terminal: decodeSharedTerminal,
  permissions: decodePermissions,
  expiresAt: str,
  csrfToken: str,
})
export type GuestSession = Decoded<typeof decodeGuestSession>

export const decodeRecording = object({
  id: str,
  terminalId: str,
  status: oneOf('recording', 'complete', 'incomplete', 'deleted'),
  formatVersion: num,
  startedAt: str,
  endedAt: nullable(str),
  durationMs: num,
  rows: num,
  cols: num,
  eventCount: num,
  chunkCount: num,
  compressedBytes: num,
  uncompressedBytes: num,
  failureCode: optional(str),
  retainUntil: nullable(str),
  deletedAt: nullable(str),
})
export type Recording = Decoded<typeof decodeRecording>

interface EventBase {
  seq: number
  offsetMs: number
}
export type PlaybackEvent =
  | (EventBase & { kind: 'output'; data: Uint8Array })
  | (EventBase & { kind: 'resize'; rows: number; cols: number })
  | (EventBase & {
      kind: 'lifecycle'
      state: string
      exitCode?: number
      signal?: string
      reason?: string
    })
  | (EventBase & {
      kind: 'presence'
      event: string
      participantId: string
      role: string
      reason?: string
    })

export function base64ToBytes(value: string): Uint8Array {
  const binary = atob(value)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return bytes
}

const eventHeader = object({
  seq: num,
  offsetMs: num,
  kind: oneOf('output', 'resize', 'lifecycle', 'presence'),
})
const outputData = object({ data: str })
const resizeData = object({ rows: num, cols: num })
const lifecycleData = object({
  state: str,
  exitCode: optional(num),
  signal: optional(str),
  reason: optional(str),
})
const presenceData = object({
  event: str,
  participantId: str,
  role: str,
  reason: optional(str),
})

export const decodePlaybackEvent: Decoder<PlaybackEvent> = (
  value,
  path = '',
) => {
  const { seq, offsetMs, kind } = eventHeader(value, path)
  const data = (value as { data: unknown }).data
  const at = `${path}.data`
  switch (kind) {
    case 'output': {
      const raw = outputData(data, at).data
      try {
        return { seq, offsetMs, kind, data: base64ToBytes(raw) }
      } catch {
        throw new DecodeError(at)
      }
    }
    case 'resize':
      return { seq, offsetMs, kind, ...resizeData(data, at) }
    case 'lifecycle': {
      const d = lifecycleData(data, at)
      return {
        seq,
        offsetMs,
        kind,
        state: d.state,
        ...definedOnly({
          exitCode: d.exitCode,
          signal: d.signal,
          reason: d.reason,
        }),
      }
    }
    case 'presence': {
      const d = presenceData(data, at)
      return {
        seq,
        offsetMs,
        kind,
        event: d.event,
        participantId: d.participantId,
        role: d.role,
        ...definedOnly({ reason: d.reason }),
      }
    }
  }
}

function definedOnly<T extends Record<string, unknown>>(values: T): Partial<T> {
  return Object.fromEntries(
    Object.entries(values).filter(([, v]) => v !== undefined),
  ) as Partial<T>
}

export const decodeCursor = object({ afterMs: num, afterSeq: num })
export type PlaybackCursor = Decoded<typeof decodeCursor>

export const decodeEventsPage = object({
  recording: decodeRecording,
  events: arrayOf(decodePlaybackEvent),
  next: nullable(decodeCursor),
})
export type EventsPage = Decoded<typeof decodeEventsPage>

export const decodeAuditEvent = object({
  id: str,
  type: str,
  occurredAt: str,
  remoteAddr: str,
  details: stringMap,
})
export type AuditEvent = Decoded<typeof decodeAuditEvent>

export const decodeAuditPage = object({
  events: arrayOf(decodeAuditEvent),
  nextCursor: nullable(str),
})
export type AuditPage = Decoded<typeof decodeAuditPage>

export const decodeSetting = object({
  key: str,
  group: str,
  label: str,
  value: str,
  restartRequired: bool,
})
export type Setting = Decoded<typeof decodeSetting>
export const decodeSettings = object({
  readOnly: bool,
  settings: arrayOf(decodeSetting),
})
export type RuntimeSettings = Decoded<typeof decodeSettings>
