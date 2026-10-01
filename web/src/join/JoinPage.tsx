import { LogOut } from 'lucide-react'
import { type ReactNode, useEffect, useState } from 'react'

import { ApiError } from '../api/client'
import { useServices } from '../api/services'
import type { GuestSession } from '../api/types'
import { AuthFrame } from '../auth/AuthFrame'
import { InlineError, Loading } from '../components/StateViews'
import { toApiError } from '../lib/useAsync'
import type { ConnectionState, EndReason, ProbeResult } from '../terminal/connection'
import { Workspace } from '../terminal/Workspace'
import { beginJoin, endJoin } from './invite'

type JoinState =
  | { kind: 'joining' }
  | { kind: 'joined'; session: GuestSession }
  | { kind: 'failed'; error: ApiError }
  | { kind: 'left' }

const endedCopy: Partial<Record<EndReason, string>> = {
  exited: 'The terminal ended. There is nothing more to watch.',
  revoked: 'The administrator revoked this link. Ask them for a new one if you still need access.',
  replaced: 'A new editor link replaced this one. Ask the administrator for a new link if you still need access.',
  superseded: 'You opened another invitation in this browser, so this view closed. Continue in the tab you just opened.',
  expired: 'This link expired. Ask the administrator for a new one if you still need access.',
  logged_out: 'You left this terminal from another tab.',
  unauthorized: 'Your access ended. Open a new link to join again.',
  not_found: 'This terminal no longer exists.',
  closed: 'The connection was closed. Reload the page to try again.',
}

function failureCopy(error: ApiError): { title: string; body: ReactNode } {
  switch (error.code) {
    case 'invalid_invitation':
      return {
        title: 'This link no longer works',
        body: 'It may have expired, been revoked, or already been used. Ask the person who shared it for a new link.',
      }
    case 'invalid_state':
      return { title: 'This terminal has ended', body: 'The terminal this link was for is no longer running.' }
    case 'rate_limited':
      return { title: 'Too many attempts', body: `Wait ${error.retryAfterSeconds ?? 60} seconds, then open the link again.` }
    case 'offline':
      return { title: 'The server could not be reached', body: 'Check your connection, then open the link again.' }
    case 'unauthenticated':
      return {
        title: 'Open the complete link',
        body: 'Shared links end with #token= followed by a long code. Open the full link you were sent, or ask for a new one.',
      }
    default:
      return { title: 'Joining failed', body: 'The server could not complete the request. Try the link again in a moment.' }
  }
}

export function JoinPage() {
  const { api, setCsrf } = useServices()
  const [state, setState] = useState<JoinState>({ kind: 'joining' })
  const [ended, setEnded] = useState<EndReason | null>(null)
  const [leaveError, setLeaveError] = useState(false)

  useEffect(() => {
    let active = true
    beginJoin(api.redeem, () => api.guestSession()).then(
      (session) => {
        if (!active) return
        setCsrf(session.csrfToken)
        setState({ kind: 'joined', session })
      },
      (error: unknown) => active && setState({ kind: 'failed', error: toApiError(error) }),
    )
    return () => {
      active = false
    }
  }, [api, setCsrf])

  const leave = async () => {
    setLeaveError(false)
    try {
      await api.guestLogout()
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 401)) {
        setLeaveError(true)
        return
      }
    }
    setCsrf(undefined)
    endJoin()
    setState({ kind: 'left' })
  }

  if (state.kind === 'joining') {
    return (
      <AuthFrame>
        <Loading label="Joining the terminal" />
      </AuthFrame>
    )
  }
  if (state.kind === 'left') {
    return (
      <AuthFrame>
        <h1>You left the terminal</h1>
        <p className="muted">You are signed out of this shared terminal. Open the link again to rejoin while it is still valid.</p>
      </AuthFrame>
    )
  }
  if (state.kind === 'failed') {
    const copy = failureCopy(state.error)
    return (
      <AuthFrame>
        <h1>{copy.title}</h1>
        <p className="muted">{copy.body}</p>
      </AuthFrame>
    )
  }

  const { session } = state
  const probe = async (): Promise<ProbeResult> => {
    try {
      const current = await api.guestSession()
      return current.terminal.state === 'running' || current.terminal.state === 'starting' ? 'retry' : { ended: 'exited' }
    } catch (e) {
      return e instanceof ApiError && e.status === 401 ? { ended: 'expired' } : 'retry'
    }
  }
  const onState = (next: ConnectionState) => setEnded(next.kind === 'ended' ? next.reason : null)

  return (
    <Workspace
      terminalId={session.terminal.id}
      initialSize={{ rows: session.terminal.rows, cols: session.terminal.cols }}
      probe={probe}
      onState={onState}
      header={
        <>
          <p className="wordmark">webpty</p>
          <h1 className="workspace__command">{session.role === 'editor' ? 'You joined as an editor.' : 'You joined as a viewer.'}</h1>
        </>
      }
      actions={
        <>
          <button type="button" className="btn" onClick={leave}>
            <LogOut aria-hidden size={14} /> Leave
          </button>
          {leaveError && <InlineError>Leaving failed. Try again.</InlineError>}
        </>
      }
      rail={
        ended && endedCopy[ended] ? (
          <section className="rail-section" aria-labelledby="ended-h">
            <h2 id="ended-h">Disconnected</h2>
            <p>{endedCopy[ended]}</p>
          </section>
        ) : null
      }
    />
  )
}
