import { describe, expect, test } from 'vitest'

import { ApiError, createApiClient, isAbortError } from './client'
import { object, str } from './decode'

type Call = { url: string; init: RequestInit }

function fakeFetch(respond: (call: Call) => Response | Promise<Response>) {
  const calls: Call[] = []
  const fetchImpl = (async (url: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: String(url), init: init ?? {} }
    calls.push(call)
    return respond(call)
  }) as typeof fetch
  return { fetchImpl, calls }
}

function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

const idOnly = object({ id: str })

describe('api client', () => {
  test('reads JSON with same-origin credentials and no CSRF header', async () => {
    const { fetchImpl, calls } = fakeFetch(() => json(200, { id: 't1' }))
    const client = createApiClient({ fetch: fetchImpl, csrf: () => 'csrf-1' })

    await expect(client.get('/api/v1/admin/terminals/t1', idOnly)).resolves.toEqual({ id: 't1' })

    const headers = new Headers(calls[0].init.headers)
    expect(calls[0].init.method).toBe('GET')
    expect(calls[0].init.credentials).toBe('same-origin')
    expect(headers.get('Accept')).toBe('application/json')
    expect(headers.has('X-CSRF-Token')).toBe(false)
  })

  test('mutations send JSON and the session CSRF token', async () => {
    const { fetchImpl, calls } = fakeFetch(() => json(201, { id: 't2' }))
    const client = createApiClient({ fetch: fetchImpl, csrf: () => 'csrf-2' })

    await client.post('/api/v1/admin/terminals', { rows: 24, cols: 80 }, idOnly)

    const headers = new Headers(calls[0].init.headers)
    expect(calls[0].init.method).toBe('POST')
    expect(headers.get('Content-Type')).toBe('application/json')
    expect(headers.get('X-CSRF-Token')).toBe('csrf-2')
    expect(calls[0].init.body).toBe('{"rows":24,"cols":80}')
  })

  test('no content responses resolve to undefined', async () => {
    const { fetchImpl } = fakeFetch(() => new Response(null, { status: 204 }))
    const client = createApiClient({ fetch: fetchImpl })
    await expect(client.post('/api/v1/admin/logout', undefined)).resolves.toBeUndefined()
  })

  test('coded server errors keep status, code, and the fixed server message', async () => {
    const { fetchImpl } = fakeFetch(() =>
      json(410, { error: 'recording was deleted', code: 'recording_deleted' }),
    )
    const client = createApiClient({ fetch: fetchImpl })

    const error = await client.get('/api/v1/admin/recordings/r1', idOnly).catch((e: unknown) => e)

    expect(error).toBeInstanceOf(ApiError)
    expect(error).toMatchObject({ status: 410, code: 'recording_deleted', message: 'recording was deleted' })
  })

  test.each([
    [401, 'unauthenticated', 'unauthenticated'],
    [403, 'password change required', 'password_change_required'],
    [403, 'invalid CSRF token', 'csrf'],
    [403, 'untrusted host; configure WEBPTY_PUBLIC_ORIGIN', 'untrusted_host'],
    [401, 'invalid credentials', 'invalid_credentials'],
    [403, 'invalid current password', 'invalid_current_password'],
    [404, 'not found', 'not_found'],
    [500, 'internal error', 'internal'],
  ])('uncoded %i %s maps to %s', async (status, message, code) => {
    const { fetchImpl } = fakeFetch(() => json(status, { error: message }))
    const client = createApiClient({ fetch: fetchImpl })
    await expect(client.get('/x', idOnly)).rejects.toMatchObject({ status, code })
  })

  test('throttling exposes Retry-After seconds', async () => {
    const { fetchImpl } = fakeFetch(() => json(429, { error: 'too many attempts' }, { 'Retry-After': '60' }))
    const client = createApiClient({ fetch: fetchImpl })
    await expect(client.post('/api/v1/admin/login', { password: 'x' })).rejects.toMatchObject({
      code: 'rate_limited',
      retryAfterSeconds: 60,
    })
  })

  test('network failures become an offline error without echoing the request body', async () => {
    const { fetchImpl } = fakeFetch(() => {
      throw new TypeError('Failed to fetch')
    })
    const client = createApiClient({ fetch: fetchImpl })

    const error = (await client
      .post('/api/v1/admin/login', { password: 'hunter2-secret' })
      .catch((e: unknown) => e)) as ApiError

    expect(error).toMatchObject({ status: 0, code: 'offline' })
    expect(JSON.stringify({ ...error, message: error.message })).not.toContain('hunter2-secret')
  })

  test('aborts propagate as abort errors, not API errors', async () => {
    const { fetchImpl } = fakeFetch(({ init }) => {
      if (init.signal?.aborted) throw new DOMException('aborted', 'AbortError')
      return json(200, { id: 'x' })
    })
    const client = createApiClient({ fetch: fetchImpl })
    const controller = new AbortController()
    controller.abort()

    const error = await client.get('/x', idOnly, controller.signal).catch((e: unknown) => e)

    expect(isAbortError(error)).toBe(true)
    expect(error).not.toBeInstanceOf(ApiError)
  })

  test('responses that do not match the expected shape are rejected', async () => {
    const { fetchImpl } = fakeFetch(() => json(200, { id: 42 }))
    const client = createApiClient({ fetch: fetchImpl })
    await expect(client.get('/x', idOnly)).rejects.toMatchObject({ code: 'invalid_response' })
  })

  test('non-JSON error bodies still produce a typed error', async () => {
    const { fetchImpl } = fakeFetch(() => new Response('404 page not found\n', { status: 404 }))
    const client = createApiClient({ fetch: fetchImpl })
    await expect(client.get('/x', idOnly)).rejects.toMatchObject({ status: 404, code: 'not_found' })
  })
})
