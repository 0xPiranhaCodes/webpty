import { describe, expect, test } from 'vitest'

import * as fx from '../test/fixtures'
import { createApiClient } from './client'
import { createApi } from './endpoints'

function harness(routes: Record<string, { status?: number; body?: unknown }>) {
  const calls: { method: string; url: string; body?: string }[] = []
  const fetchImpl = (async (url: RequestInfo | URL, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url: String(url), body: init?.body as string | undefined })
    const route = routes[`${method} ${String(url)}`]
    if (!route) return new Response('404 page not found', { status: 404 })
    if (route.body === undefined) return new Response(null, { status: route.status ?? 204 })
    return new Response(JSON.stringify(route.body), {
      status: route.status ?? 200,
      headers: { 'Content-Type': 'application/json' },
    })
  }) as typeof fetch
  return { api: createApi(createApiClient({ fetch: fetchImpl, csrf: () => 'csrf' })), calls }
}

describe('api endpoints', () => {
  test('admin session states decode', async () => {
    const { api } = harness({ 'GET /api/v1/admin/session': { body: fx.bootstrapSession } })
    await expect(api.adminSession()).resolves.toMatchObject({ state: 'bootstrap', passwordChangeRequired: true })
  })

  test('terminal lifecycle routes', async () => {
    const { api, calls } = harness({
      'GET /api/v1/admin/terminals': { body: { sessions: [fx.terminalRunning, fx.terminalExited] } },
      'POST /api/v1/admin/terminals': { status: 201, body: fx.terminalRunning },
      'DELETE /api/v1/admin/terminals/tm_3kq9x2': { body: { ...fx.terminalRunning, state: 'terminated' } },
    })

    const list = await api.listTerminals()
    expect(list.map((t) => t.state)).toEqual(['running', 'exited'])
    expect(list[1].exitCode).toBe(0)

    await api.createTerminal({ command: '/bin/zsh', args: ['-l'], rows: 32, cols: 120, record: false })
    expect(JSON.parse(calls[1].body!)).toEqual({ command: '/bin/zsh', args: ['-l'], rows: 32, cols: 120, record: false })

    await expect(api.terminateTerminal('tm_3kq9x2')).resolves.toMatchObject({ state: 'terminated' })
  })

  test('create terminal omits empty fields so server defaults apply', async () => {
    const { api, calls } = harness({ 'POST /api/v1/admin/terminals': { status: 201, body: fx.terminalRunning } })
    await api.createTerminal({ command: '', args: [], record: true })
    expect(JSON.parse(calls[0].body!)).toEqual({})
  })

  test('path segments are escaped', async () => {
    const { api, calls } = harness({})
    await api.getTerminal('../../admin/logout').catch(() => undefined)
    expect(calls[0].url).toBe('/api/v1/admin/terminals/..%2F..%2Fadmin%2Flogout')
  })

  test('grant creation returns the one-time invitation', async () => {
    const { api, calls } = harness({
      'POST /api/v1/admin/terminals/tm_3kq9x2/grants': { status: 201, body: fx.createdGrant },
      'GET /api/v1/admin/terminals/tm_3kq9x2/grants': { body: { grants: [fx.editorGrant, fx.viewerGrant] } },
      'DELETE /api/v1/admin/terminals/tm_3kq9x2/grants/gr_viewer1': { body: { ...fx.viewerGrant, status: 'revoked' } },
    })

    const created = await api.createGrant('tm_3kq9x2', { role: 'editor', label: 'Pairing', ttlSeconds: 3600, singleUse: true })
    expect(created.inviteUrl).toContain('/join#token=')
    expect(created.replaced).toEqual(['gr_oldeditor'])
    expect(JSON.parse(calls[0].body!)).toEqual({ role: 'editor', label: 'Pairing', ttlSeconds: 3600, singleUse: true })

    const grants = await api.listGrants('tm_3kq9x2')
    expect(grants.map((g) => g.role)).toEqual(['editor', 'viewer'])
    expect(JSON.stringify(grants)).not.toContain(fx.inviteToken)

    await expect(api.revokeGrant('tm_3kq9x2', 'gr_viewer1')).resolves.toMatchObject({ status: 'revoked' })
  })

  test('guest redemption and session', async () => {
    const { api, calls } = harness({
      'POST /api/v1/access/redeem': { body: fx.guestSession },
      'GET /api/v1/access/session': { body: fx.guestSession },
      'POST /api/v1/access/logout': {},
    })
    await expect(api.redeem(fx.inviteToken)).resolves.toMatchObject({ role: 'viewer', permissions: { input: false } })
    expect(JSON.parse(calls[0].body!)).toEqual({ token: fx.inviteToken })
    await expect(api.guestSession()).resolves.toMatchObject({ terminal: { id: fx.terminalRunning.id } })
    await expect(api.guestLogout()).resolves.toBeUndefined()
  })

  test('recordings, playback pages, retention', async () => {
    const { api, calls } = harness({
      'GET /api/v1/admin/recordings?limit=100': { body: { recordings: [fx.recordingComplete, fx.recordingIncomplete] } },
      'GET /api/v1/admin/recordings/rc_91ab/events?afterMs=0&afterSeq=0&limit=500': { body: fx.recordingEventsPage },
      'DELETE /api/v1/admin/recordings/rc_91ab': { body: { ...fx.recordingComplete, status: 'deleted', deletedAt: '2026-10-01T05:00:00Z' } },
      'POST /api/v1/admin/recordings/retention/run': { body: { deleted: 2 } },
    })

    const list = await api.listRecordings()
    expect(list[1]).toMatchObject({ status: 'incomplete', failureCode: 'queue_overflow' })

    const page = await api.recordingEvents('rc_91ab', { afterMs: 0, afterSeq: 0 })
    const output = page.events[1]
    expect(output).toMatchObject({ seq: 2, offsetMs: 0, kind: 'output' })
    expect(output.kind === 'output' && Array.from(output.data)).toEqual([36, 32])
    expect(page.events[4]).toEqual({ seq: 5, offsetMs: 2000, kind: 'resize', rows: 30, cols: 100 })
    expect(page.next).toBeNull()

    await expect(api.deleteRecording('rc_91ab')).resolves.toMatchObject({ status: 'deleted' })
    await expect(api.runRetention()).resolves.toBe(2)
    expect(calls.at(-1)?.method).toBe('POST')
    expect(api.exportUrl('rc_91ab')).toBe('/api/v1/admin/recordings/rc_91ab/export')
  })

  test('audit pages and settings', async () => {
    const { api } = harness({
      'GET /api/v1/admin/audit?limit=50': { body: fx.auditPage },
      'GET /api/v1/admin/audit?limit=50&cursor=41': { body: { events: [], nextCursor: null } },
      'GET /api/v1/admin/settings': { body: fx.settings },
    })
    const first = await api.auditPage()
    expect(first.events[0].details.grantId).toBe('gr_editor1')
    expect(first.nextCursor).toBe('41')
    await expect(api.auditPage('41')).resolves.toEqual({ events: [], nextCursor: null })
    await expect(api.settings()).resolves.toMatchObject({ readOnly: true })
  })
})
