export interface FakeRequest {
  method: string
  path: string
  url: URL
  body: unknown
  headers: Headers
}

export interface FakeResponse {
  status?: number
  body?: unknown
  headers?: Record<string, string>
}

export type Route = FakeResponse | ((request: FakeRequest) => FakeResponse | Promise<FakeResponse>)

/**
 * An in-memory stand-in for the Go server: routes are keyed by
 * "METHOD /path" (optionally with the query string) and answer with the
 * same JSON shapes the handlers produce.
 */
export function createFakeServer(initial: Record<string, Route> = {}) {
  const routes = new Map(Object.entries(initial))
  const requests: FakeRequest[] = []

  const fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://127.0.0.1:8000')
    const method = init?.method ?? 'GET'
    const request: FakeRequest = {
      method,
      path: url.pathname,
      url,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
      headers: new Headers(init?.headers),
    }
    requests.push(request)
    const route = routes.get(`${method} ${url.pathname}${url.search}`) ?? routes.get(`${method} ${url.pathname}`)
    if (!route) return new Response('404 page not found\n', { status: 404 })
    const response = typeof route === 'function' ? await route(request) : route
    if (response.body === undefined) return new Response(null, { status: response.status ?? 204, headers: response.headers })
    return new Response(JSON.stringify(response.body), {
      status: response.status ?? 200,
      headers: { 'Content-Type': 'application/json', ...response.headers },
    })
  }) as typeof globalThis.fetch

  return {
    fetch,
    requests,
    set(key: string, route: Route) {
      routes.set(key, route)
    },
    calls(key: string) {
      const [method, path] = key.split(' ')
      return requests.filter((r) => r.method === method && r.path === path)
    },
  }
}

export type FakeServer = ReturnType<typeof createFakeServer>
