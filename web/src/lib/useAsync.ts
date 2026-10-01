import { useCallback, useEffect, useRef, useState } from 'react'

import { ApiError, isAbortError } from '../api/client'

export type AsyncState<T> =
  | { status: 'loading'; data?: T }
  | { status: 'ready'; data: T }
  | { status: 'error'; error: ApiError; data?: T }

/** Loads data when key changes, aborting stale requests. */
export function useAsync<T>(key: string, load: (signal: AbortSignal) => Promise<T>) {
  const [state, setState] = useState<AsyncState<T>>({ status: 'loading' })
  const [nonce, setNonce] = useState(0)
  const loadRef = useRef(load)
  useEffect(() => {
    loadRef.current = load
  })

  useEffect(() => {
    const controller = new AbortController()
    setState((previous) => ({ status: 'loading', data: previous.data }))
    loadRef.current(controller.signal).then(
      (data) => !controller.signal.aborted && setState({ status: 'ready', data }),
      (error: unknown) => {
        if (controller.signal.aborted || isAbortError(error)) return
        const apiError = error instanceof ApiError ? error : new ApiError(0, 'internal', 'something went wrong')
        setState((previous) => ({ status: 'error', error: apiError, data: previous.data }))
      },
    )
    return () => controller.abort()
  }, [key, nonce])

  const reload = useCallback(() => setNonce((n) => n + 1), [])
  return { ...state, reload }
}

export function toApiError(error: unknown): ApiError {
  return error instanceof ApiError ? error : new ApiError(0, 'internal', 'something went wrong')
}
