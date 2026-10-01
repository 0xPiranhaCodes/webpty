// Payloads shaped exactly like the Go handlers' JSON (internal/httpapi).

export const terminalRunning = {
  id: 'tm_3kq9x2',
  state: 'running',
  command: '/bin/zsh',
  args: ['-l'],
  rows: 32,
  cols: 120,
  createdAt: '2026-10-01T04:02:11Z',
  startedAt: '2026-10-01T04:02:11Z',
  endedAt: null,
  lastActivityAt: '2026-10-01T04:10:40Z',
  exitCode: null,
  participants: 2,
}

export const terminalExited = {
  id: 'tm_7hd01a',
  state: 'exited',
  command: '/bin/sh',
  args: [],
  rows: 24,
  cols: 80,
  createdAt: '2026-09-30T21:14:00Z',
  startedAt: '2026-09-30T21:14:00Z',
  endedAt: '2026-09-30T21:40:12Z',
  lastActivityAt: '2026-09-30T21:40:12Z',
  exitCode: 0,
  participants: 0,
}

export const adminSession = {
  state: 'authenticated',
  passwordChangeRequired: false,
  csrfToken: 'csrf-admin-token',
  expiresAt: '2026-10-01T16:00:00Z',
}

export const bootstrapSession = {
  state: 'bootstrap',
  passwordChangeRequired: true,
  csrfToken: 'csrf-bootstrap-token',
  expiresAt: '2026-10-01T04:20:00Z',
}

export const editorGrant = {
  id: 'gr_editor1',
  terminalId: terminalRunning.id,
  role: 'editor',
  label: 'Pairing with Ana',
  singleUse: true,
  maxRedemptions: 1,
  redemptionCount: 0,
  status: 'active',
  createdAt: '2026-10-01T04:05:00Z',
  updatedAt: '2026-10-01T04:05:00Z',
  expiresAt: '2026-10-02T04:05:00Z',
  redeemedAt: null,
  revokedAt: null,
}

export const viewerGrant = {
  ...editorGrant,
  id: 'gr_viewer1',
  role: 'viewer',
  label: '',
  singleUse: false,
  maxRedemptions: 100,
  redemptionCount: 3,
}

export const inviteToken = 'wpi_Z2l2ZS1tZS1hLXNlY3JldA'

export const createdGrant = {
  grant: editorGrant,
  token: inviteToken,
  inviteUrl: `http://127.0.0.1:8000/join#token=${inviteToken}`,
  replaced: ['gr_oldeditor'],
}

export const guestSession = {
  role: 'viewer',
  grantId: viewerGrant.id,
  terminal: {
    id: terminalRunning.id,
    state: 'running',
    rows: 32,
    cols: 120,
    createdAt: terminalRunning.createdAt,
    startedAt: terminalRunning.startedAt,
    endedAt: null,
    exitCode: null,
  },
  permissions: { input: false, resize: false },
  expiresAt: '2026-10-01T16:05:00Z',
  csrfToken: 'csrf-guest-token',
}

export const recordingComplete = {
  id: 'rc_91ab',
  terminalId: terminalExited.id,
  status: 'complete',
  formatVersion: 1,
  startedAt: '2026-09-30T21:14:00Z',
  endedAt: '2026-09-30T21:40:12Z',
  durationMs: 1_572_000,
  rows: 24,
  cols: 80,
  eventCount: 4,
  chunkCount: 1,
  compressedBytes: 812,
  uncompressedBytes: 2048,
  retainUntil: '2026-10-30T21:40:12Z',
  deletedAt: null,
}

export const recordingIncomplete = {
  ...recordingComplete,
  id: 'rc_22cd',
  status: 'incomplete',
  failureCode: 'queue_overflow',
}

const b64 = (text: string) => btoa(text)

export const recordingEventsPage = {
  recording: recordingComplete,
  events: [
    { seq: 1, offsetMs: 0, kind: 'lifecycle', data: { state: 'running' } },
    { seq: 2, offsetMs: 0, kind: 'output', data: { data: b64('$ ') } },
    { seq: 3, offsetMs: 1200, kind: 'output', data: { data: b64('ls\r\n') } },
    { seq: 4, offsetMs: 1500, kind: 'presence', data: { event: 'joined', participantId: 'pt_1', role: 'viewer' } },
    { seq: 5, offsetMs: 2000, kind: 'resize', data: { rows: 30, cols: 100 } },
    { seq: 6, offsetMs: 2500, kind: 'output', data: { data: b64('README.md\r\n') } },
    { seq: 7, offsetMs: 4000, kind: 'lifecycle', data: { state: 'exited', exitCode: 0 } },
  ],
  next: null,
}

export const auditPage = {
  events: [
    {
      id: '42',
      type: 'access.grant.created',
      occurredAt: '2026-10-01T04:05:00Z',
      remoteAddr: '127.0.0.1',
      details: { terminalId: terminalRunning.id, grantId: editorGrant.id, role: 'editor' },
    },
    { id: '41', type: 'admin.login', occurredAt: '2026-10-01T04:01:00Z', remoteAddr: '127.0.0.1', details: {} },
  ],
  nextCursor: '41',
}

export const settings = {
  readOnly: true,
  settings: [
    { key: 'WEBPTY_ADDRESS', group: 'Server', label: 'Listen address', value: '127.0.0.1:8000', restartRequired: true },
    { key: 'WEBPTY_PUBLIC_ORIGIN', group: 'Server', label: 'Public origin', value: 'Not set (loopback only)', restartRequired: true },
    { key: 'WEBPTY_RECORDING_ENABLED', group: 'Recording', label: 'Record new terminals', value: 'true', restartRequired: true },
    { key: 'WEBPTY_RECORDING_RETENTION', group: 'Recording', label: 'Retention', value: '720h0m0s', restartRequired: true },
  ],
}
