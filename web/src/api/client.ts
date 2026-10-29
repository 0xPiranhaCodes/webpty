import { DecodeError, type Decoder } from './decode'

// Admin auth errors carry fixed messages but no code; these map them to
// stable codes the UI can branch on.
const messageCodes: Record<string, string> = {
  'password change required': 'password_change_required',
  'invalid CSRF token': 'csrf',
  'cross-origin request rejected': 'cross_origin',
  'untrusted host; configure WEBPTY_PUBLIC_ORIGIN': 'untrusted_host',
  'invalid credentials': 'invalid_credentials',
  'invalid current password': 'invalid_current_password',
  'too many attempts': 'rate_limited',
  'access is not valid for this terminal': 'forbidden',
}

const statusCodes: Record<number, string> = {
  400: 'invalid_argument',
  401: 'unauthenticated',
  403: 'forbidden',
  404: 'not_found',
  409: 'conflict',
  410: 'gone',
  413: 'too_large',
  415: 'unsupported_media_type',
  422: 'unprocessable',
  429: 'rate_limited',
  503: 'unavailable',
}

const httpNoContent = 204
const httpServerErrorMin = 500

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly retryAfterSeconds?: number

  constructor(
    status: number,
    code: string,
    message: string,
    retryAfterSeconds?: number,
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.retryAfterSeconds = retryAfterSeconds
  }
}

const httpBadRequest = 400
const httpUnauthorized = 401
const httpNotFound = 404

export const isApiStatus = (error: unknown, status: number) =>
  error instanceof ApiError && error.status === status
export const isUnauthorized = (error: unknown) =>
  isApiStatus(error, httpUnauthorized)
export const isNotFound = (error: unknown) => isApiStatus(error, httpNotFound)
export const isBadRequest = (error: unknown) =>
  isApiStatus(error, httpBadRequest)

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

export interface ApiClientOptions {
  fetch?: typeof fetch
  /** Returns the CSRF token for the current session, if any. */
  csrf?: () => string | undefined
  /** Called when an admin request is refused because the admin session is gone. */
  onAdminSessionEnded?: () => void
}

// These answer 401 as part of signing in and out, not because a session ended.
const authPaths = new Set([
  '/api/v1/admin/session',
  '/api/v1/admin/login',
  '/api/v1/admin/logout',
])

function endsAdminSession(path: string, error: ApiError) {
  const pathname = path.split('?')[0]
  return (
    error.status === httpUnauthorized &&
    error.code === 'unauthenticated' &&
    pathname.startsWith('/api/v1/admin/') &&
    !authPaths.has(pathname)
  )
}

export interface ApiClient {
  get<T>(path: string, decode: Decoder<T>, signal?: AbortSignal): Promise<T>
  post<T = undefined>(
    path: string,
    body: unknown,
    decode?: Decoder<T>,
    signal?: AbortSignal,
  ): Promise<T>
  del<T = undefined>(
    path: string,
    decode?: Decoder<T>,
    signal?: AbortSignal,
  ): Promise<T>
}

async function errorFrom(response: Response): Promise<ApiError> {
  let message = ''
  let code = ''
  try {
    const body: unknown = await response.json()
    if (body && typeof body === 'object') {
      const record = body as Record<string, unknown>
      if (typeof record.error === 'string') message = record.error
      if (typeof record.code === 'string') code = record.code
    }
  } catch {
    // Non-JSON bodies (for example the router's plain 404) carry no detail.
  }
  code ||=
    messageCodes[message] ??
    statusCodes[response.status] ??
    (response.status >= httpServerErrorMin ? 'internal' : 'error')
  const retry = Number(response.headers.get('Retry-After'))
  return new ApiError(
    response.status,
    code,
    message || `request failed (${response.status})`,
    Number.isFinite(retry) && retry > 0 ? retry : undefined,
  )
}

function requestHeaders(
  method: string,
  body: unknown,
  csrf?: () => string | undefined,
) {
  const headers = new Headers({ Accept: 'application/json' })
  if (method !== 'GET') {
    const token = csrf?.()
    if (token) headers.set('X-CSRF-Token', token)
  }
  if (body !== undefined) headers.set('Content-Type', 'application/json')
  return headers
}

// Any failure to reach the server, other than a caller's abort, becomes one 'offline' error.
async function send(
  fetchImpl: typeof fetch,
  method: string,
  path: string,
  body: unknown,
  headers: Headers,
  signal: AbortSignal | undefined,
): Promise<Response> {
  try {
    return await fetchImpl(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
      signal,
    })
  } catch (error) {
    if (isAbortError(error) || signal?.aborted) throw error
    throw new ApiError(0, 'offline', 'the server could not be reached')
  }
}

async function readJson(response: Response): Promise<unknown> {
  try {
    return await response.json()
  } catch (error) {
    if (isAbortError(error)) throw error
    throw new ApiError(
      response.status,
      'invalid_response',
      'the server sent an unreadable response',
    )
  }
}

function decodePayload<T>(
  response: Response,
  payload: unknown,
  decode: Decoder<T>,
): T {
  try {
    return decode(payload)
  } catch (error) {
    if (error instanceof DecodeError) {
      throw new ApiError(
        response.status,
        'invalid_response',
        `the server sent an unexpected response (${error.path})`,
      )
    }
    throw error
  }
}

export function createApiClient(options: ApiClientOptions = {}): ApiClient {
  const fetchImpl =
    options.fetch ??
    ((...args: Parameters<typeof fetch>) => globalThis.fetch(...args))

  async function request<T>(
    method: string,
    path: string,
    body: unknown,
    decode: Decoder<T> | undefined,
    signal: AbortSignal | undefined,
  ): Promise<T> {
    const headers = requestHeaders(method, body, options.csrf)
    const response = await send(fetchImpl, method, path, body, headers, signal)
    if (!response.ok) {
      const error = await errorFrom(response)
      if (endsAdminSession(path, error)) options.onAdminSessionEnded?.()
      throw error
    }
    if (!decode || response.status === httpNoContent) return undefined as T
    return decodePayload(response, await readJson(response), decode)
  }

  return {
    get: (path, decode, signal) =>
      request('GET', path, undefined, decode, signal),
    post: (path, body, decode, signal) =>
      request('POST', path, body, decode, signal),
    del: (path, decode, signal) =>
      request('DELETE', path, undefined, decode, signal),
  }
}
