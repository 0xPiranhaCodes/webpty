import { Outlet } from 'react-router'

import { Loading } from '../components/StateViews'
import { useAdminAuth } from './authContext'
import { AuthFrame } from './AuthFrame'
import { ChangePasswordPage } from './ChangePasswordPage'
import { LoginPage } from './LoginPage'

/**
 * Renders admin routes only for a fully authenticated administrator. A
 * bootstrap session sees nothing but the password change.
 */
export function RequireAdmin() {
  const { state } = useAdminAuth()
  switch (state.status) {
    case 'loading':
      return (
        <AuthFrame>
          <Loading label="Checking your session" />
        </AuthFrame>
      )
    case 'anonymous':
    case 'error':
      return <LoginPage />
    case 'bootstrap':
      return <ChangePasswordPage />
    case 'authenticated':
      return <Outlet />
  }
}
