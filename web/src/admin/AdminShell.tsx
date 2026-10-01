import { LogOut } from 'lucide-react'
import { useEffect, useState } from 'react'
import { NavLink, Outlet } from 'react-router'

import { useApi } from '../api/services'
import { useAdminAuth } from '../auth/authContext'

const links = [
  { to: '/admin', label: 'Overview', end: true },
  { to: '/admin/sessions', label: 'Live sessions', live: true },
  { to: '/admin/recordings', label: 'Recordings' },
  { to: '/admin/access', label: 'Access grants' },
  { to: '/admin/audit', label: 'Audit log' },
  { to: '/admin/settings', label: 'Settings' },
]

const pollMs = 15_000

/** Number of running terminals, refreshed while the shell is open. */
function useRunningCount(): number | null {
  const api = useApi()
  const [count, setCount] = useState<number | null>(null)
  useEffect(() => {
    const controller = new AbortController()
    const load = () =>
      api.listTerminals(controller.signal).then(
        (terminals) => setCount(terminals.filter((t) => t.state === 'running').length),
        () => {},
      )
    void load()
    const timer = setInterval(load, pollMs)
    return () => {
      controller.abort()
      clearInterval(timer)
    }
  }, [api])
  return count
}

export function AdminShell() {
  const auth = useAdminAuth()
  const running = useRunningCount()
  const [signOutError, setSignOutError] = useState(false)

  const signOut = async () => {
    setSignOutError(false)
    try {
      await auth.logout()
    } catch {
      setSignOutError(true)
    }
  }

  return (
    <div className="shell">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <header className="shell__nav">
        <p className="wordmark">webpty</p>
        <nav aria-label="Admin">
          <ul className="nav">
            {links.map((link) => (
              <li key={link.to}>
                <NavLink
                  to={link.to}
                  end={link.end}
                  className="nav__link"
                  data-live={link.live && running ? 'true' : undefined}
                >
                  <span>{link.label}</span>
                  {link.live && running ? (
                    <span className="nav__count">
                      {running}
                      <span className="visually-hidden"> running</span>
                    </span>
                  ) : null}
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>
        <div className="shell__account">
          <button type="button" className="btn btn--quiet" onClick={signOut}>
            <LogOut aria-hidden size={14} /> Sign out
          </button>
          {signOutError && (
            <p className="inline-error" role="alert">
              Sign-out failed. Try again.
            </p>
          )}
        </div>
      </header>
      <main className="shell__main" id="main" tabIndex={-1}>
        <Outlet />
      </main>
    </div>
  )
}
