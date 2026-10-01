import type { ApiClient } from './client'
import { arrayOf, nullable, num, object, optional, str } from './decode'
import {
  decodeAdminSession,
  decodeAuditPage,
  decodeCreatedGrant,
  decodeEventsPage,
  decodeGrant,
  decodeGuestSession,
  decodeRecording,
  decodeSettings,
  decodeTerminal,
  type GrantRole,
  type PlaybackCursor,
} from './types'

const seg = encodeURIComponent

const terminalList = object({ sessions: arrayOf(decodeTerminal) })
const grantList = object({ grants: arrayOf(decodeGrant) })
const recordingList = object({ recordings: arrayOf(decodeRecording), nextCursor: optional(nullable(str)) })
const loginResult = object({ passwordChangeRequired: (v) => v === true })
const retentionResult = object({ deleted: num })

export interface CreateTerminalRequest {
  command: string
  args: string[]
  rows?: number
  cols?: number
  record: boolean
}

export interface CreateGrantRequest {
  role: GrantRole
  label?: string
  ttlSeconds?: number
  singleUse?: boolean
  maxRedemptions?: number
}

export const recordingPageSize = 500

export function createApi(client: ApiClient) {
  /** One page of recordings, newest first; pass nextCursor back as before for older ones. */
  const recordingsPage = async (options: { terminalId?: string; limit?: number; before?: string } = {}, signal?: AbortSignal) => {
    const query = new URLSearchParams()
    if (options.terminalId) query.set('terminalId', options.terminalId)
    query.set('limit', String(options.limit ?? 100))
    if (options.before) query.set('before', options.before)
    const page = await client.get(`/api/v1/admin/recordings?${query}`, recordingList, signal)
    return { recordings: page.recordings, nextCursor: page.nextCursor ?? null }
  }

  return {
    adminSession: (signal?: AbortSignal) => client.get('/api/v1/admin/session', decodeAdminSession, signal),
    login: (password: string) => client.post('/api/v1/admin/login', { password }, loginResult),
    changePassword: (currentPassword: string, newPassword: string) =>
      client.post('/api/v1/admin/password', { currentPassword, newPassword }),
    logout: () => client.post('/api/v1/admin/logout', undefined),

    listTerminals: async (signal?: AbortSignal) =>
      (await client.get('/api/v1/admin/terminals', terminalList, signal)).sessions,
    getTerminal: (id: string, signal?: AbortSignal) =>
      client.get(`/api/v1/admin/terminals/${seg(id)}`, decodeTerminal, signal),
    createTerminal: (request: CreateTerminalRequest) => {
      const body: Record<string, unknown> = {}
      if (request.command) body.command = request.command
      if (request.args.length) body.args = request.args
      if (request.rows) body.rows = request.rows
      if (request.cols) body.cols = request.cols
      if (!request.record) body.record = false
      return client.post('/api/v1/admin/terminals', body, decodeTerminal)
    },
    terminateTerminal: (id: string) => client.del(`/api/v1/admin/terminals/${seg(id)}`, decodeTerminal),

    listGrants: async (terminalId: string, signal?: AbortSignal) =>
      (await client.get(`/api/v1/admin/terminals/${seg(terminalId)}/grants`, grantList, signal)).grants,
    createGrant: (terminalId: string, request: CreateGrantRequest) =>
      client.post(`/api/v1/admin/terminals/${seg(terminalId)}/grants`, request, decodeCreatedGrant),
    revokeGrant: (terminalId: string, grantId: string) =>
      client.del(`/api/v1/admin/terminals/${seg(terminalId)}/grants/${seg(grantId)}`, decodeGrant),

    redeem: (token: string) => client.post('/api/v1/access/redeem', { token }, decodeGuestSession),
    guestSession: (signal?: AbortSignal) => client.get('/api/v1/access/session', decodeGuestSession, signal),
    guestLogout: () => client.post('/api/v1/access/logout', undefined),

    listRecordings: async (options: { terminalId?: string; limit?: number } = {}, signal?: AbortSignal) =>
      (await recordingsPage(options, signal)).recordings,
    recordingsPage,
    getRecording: (id: string, signal?: AbortSignal) =>
      client.get(`/api/v1/admin/recordings/${seg(id)}`, decodeRecording, signal),
    recordingEvents: (id: string, cursor: PlaybackCursor, signal?: AbortSignal) => {
      const query = new URLSearchParams({
        afterMs: String(cursor.afterMs),
        afterSeq: String(cursor.afterSeq),
        limit: String(recordingPageSize),
      })
      return client.get(`/api/v1/admin/recordings/${seg(id)}/events?${query}`, decodeEventsPage, signal)
    },
    deleteRecording: (id: string) => client.del(`/api/v1/admin/recordings/${seg(id)}`, decodeRecording),
    runRetention: async () => (await client.post('/api/v1/admin/recordings/retention/run', undefined, retentionResult)).deleted,
    exportUrl: (id: string) => `/api/v1/admin/recordings/${seg(id)}/export`,

    auditPage: (cursor?: string, signal?: AbortSignal) => {
      const query = new URLSearchParams({ limit: '50' })
      if (cursor) query.set('cursor', cursor)
      return client.get(`/api/v1/admin/audit?${query}`, decodeAuditPage, signal)
    },
    settings: (signal?: AbortSignal) => client.get('/api/v1/admin/settings', decodeSettings, signal),
  }
}

export type Api = ReturnType<typeof createApi>
