import { act } from '@testing-library/react'

import { createFakeServer, type Route } from './fakeServer'
import { FakeSocket } from './fakeTerminal'
import * as fx from './fixtures'

export function adminServer(extra: Record<string, Route> = {}) {
  return createFakeServer({
    'GET /api/v1/admin/session': { body: fx.adminSession },
    'GET /api/v1/admin/terminals': {
      body: { sessions: [fx.terminalRunning, fx.terminalExited] },
    },
    'GET /api/v1/admin/terminals/tm_3kq9x2': { body: fx.terminalRunning },
    'GET /api/v1/admin/terminals/tm_3kq9x2/grants': {
      body: { grants: [fx.editorGrant, fx.viewerGrant] },
    },
    'GET /api/v1/admin/recordings': {
      body: { recordings: [fx.recordingComplete, fx.recordingIncomplete] },
    },
    'GET /api/v1/admin/settings': { body: fx.settings },
    'GET /api/v1/admin/audit': { body: fx.auditPage },
    ...extra,
  })
}

export function readyFrame(
  role: 'owner' | 'editor' | 'viewer',
  overrides: Record<string, unknown> = {},
) {
  const canType = role !== 'viewer'
  return {
    type: 'ready',
    version: 1,
    session: {
      id: fx.terminalRunning.id,
      state: 'running',
      rows: 32,
      cols: 120,
    },
    seq: 0,
    role,
    permissions: { input: canType, resize: canType },
    ...overrides,
  }
}

/** Opens the latest fake socket and sends frames as the server would. */
export function serverSends(...frames: unknown[]) {
  act(() => {
    const socket = FakeSocket.last()
    if (socket.readyState === 0) socket.open()
    for (const frame of frames) socket.server(frame)
  })
}

export function serverCloses(code: number, reason = '') {
  act(() => FakeSocket.last().serverClose(code, reason))
}

export const b64 = (text: string) => btoa(text)
