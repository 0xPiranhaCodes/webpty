import {
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react'

import { ApiError, isAbortError } from '../api/client'
import { useServices } from '../api/services'
import { toApiError } from '../lib/useAsync'
import { type AdminAuth, AuthContext, type AuthState } from './authContext'

export function AdminAuthProvider({ children }: { children: ReactNode }) {
  const { api, setCsrf, onAdminSessionEnded } = useServices()
  const [state, setState] = useState<AuthState>({ status: 'loading' })
  // Held only in memory, only between signing in with the default password
  // and replacing it.
  const bootstrapPassword = useRef<string | null>(null)

  const refresh = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const session = await api.adminSession(signal)
        setCsrf(session.csrfToken)
        setState({
          status: session.state === 'bootstrap' ? 'bootstrap' : 'authenticated',
          session,
        })
      } catch (error) {
        if (isAbortError(error)) return
        const apiError = toApiError(error)
        setCsrf(undefined)
        setState(
          apiError.status === 401
            ? { status: 'anonymous' }
            : { status: 'error', error: apiError },
        )
      }
    },
    [api, setCsrf],
  )

  useEffect(() => {
    const controller = new AbortController()
    void refresh(controller.signal)
    return () => controller.abort()
  }, [refresh])

  const sessionEnded = useCallback(() => {
    bootstrapPassword.current = null
    setCsrf(undefined)
    setState((current) =>
      current.status === 'authenticated' || current.status === 'bootstrap'
        ? {
            status: 'anonymous',
            notice: 'Your session ended. Sign in again to continue.',
          }
        : current,
    )
  }, [setCsrf])

  useEffect(
    () => onAdminSessionEnded(sessionEnded),
    [onAdminSessionEnded, sessionEnded],
  )

  const value = useMemo<AdminAuth>(
    () => ({
      state,
      knowsBootstrapPassword: bootstrapPassword.current !== null,
      async login(password) {
        const result = await api.login(password)
        bootstrapPassword.current = result.passwordChangeRequired
          ? password
          : null
        await refresh()
      },
      async changePassword(currentPassword, newPassword) {
        const current = currentPassword ?? bootstrapPassword.current ?? ''
        await api.changePassword(current, newPassword)
        bootstrapPassword.current = null
        await refresh()
      },
      async logout() {
        try {
          await api.logout()
        } catch (error) {
          if (!(error instanceof ApiError) || error.status !== 401) throw error
        }
        bootstrapPassword.current = null
        setCsrf(undefined)
        setState({ status: 'anonymous' })
      },
      sessionEnded,
    }),
    [api, refresh, sessionEnded, setCsrf, state],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
