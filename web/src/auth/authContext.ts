import { createContext, useContext } from 'react'

import type { ApiError } from '../api/client'
import type { AdminSession } from '../api/types'

export type AuthState =
  | { status: 'loading' }
  | { status: 'anonymous'; notice?: string }
  | { status: 'bootstrap'; session: AdminSession }
  | { status: 'authenticated'; session: AdminSession }
  | { status: 'error'; error: ApiError }

export interface AdminAuth {
  state: AuthState
  /** True when the bootstrap password was just typed, so it need not be asked again. */
  knowsBootstrapPassword: boolean
  login(password: string): Promise<void>
  changePassword(
    currentPassword: string | undefined,
    newPassword: string,
  ): Promise<void>
  logout(): Promise<void>
  sessionEnded(): void
}

export const AuthContext = createContext<AdminAuth | null>(null)

export function useAdminAuth(): AdminAuth {
  const auth = useContext(AuthContext)
  if (!auth) throw new Error('useAdminAuth outside AdminAuthProvider')
  return auth
}
