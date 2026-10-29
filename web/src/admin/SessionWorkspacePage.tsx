import { ArrowLeft, Square } from 'lucide-react'
import { useState } from 'react'
import { Link, useParams } from 'react-router'

import { isNotFound, isUnauthorized } from '../api/client'
import { useApi } from '../api/services'
import { isLiveState } from '../api/types'
import { useAdminAuth } from '../auth/authContext'
import { EmptyState, ErrorState, Loading } from '../components/StateViews'
import { commandLine } from '../lib/format'
import { useAsync } from '../lib/useAsync'
import type { ProbeResult } from '../terminal/connection'
import { Workspace } from '../terminal/Workspace'
import { GrantsPanel } from './GrantsPanel'
import { TerminateDialog } from './SessionsPage'

// Only the newest few recordings can be the one still capturing a live terminal.
const activeRecordingLookupLimit = 5

export function SessionWorkspacePage() {
  const { id = '' } = useParams()
  const api = useApi()
  const auth = useAdminAuth()
  const data = useAsync(`workspace:${id}`, async (signal) => {
    const terminal = await api.getTerminal(id, signal)
    const recordings = isLiveState(terminal.state)
      ? await api
          .listRecordings(
            { terminalId: id, limit: activeRecordingLookupLimit },
            signal,
          )
          .catch(() => [])
      : []
    const active = recordings.find(
      (r) => r.terminalId === id && r.status === 'recording',
    )
    return { terminal, recordingId: active?.id ?? null }
  })
  const [terminating, setTerminating] = useState(false)

  const back = (
    <Link to="/admin/sessions" className="back-link">
      <ArrowLeft aria-hidden size={14} /> Live sessions
    </Link>
  )

  if (data.status === 'loading' && !data.data)
    return (
      <FramedState back={back}>
        <Loading label="Loading terminal" />
      </FramedState>
    )
  if (data.status === 'error' && !data.data)
    return (
      <FramedState back={back}>
        <ErrorState
          error={data.error}
          subject="terminal"
          onRetry={data.reload}
        />
      </FramedState>
    )
  const { terminal, recordingId } = data.data!
  if (!isLiveState(terminal.state)) {
    return (
      <FramedState back={back}>
        <EmptyState
          title="This terminal has ended."
          action={
            <Link
              className="btn"
              to={`/admin/recordings?terminal=${encodeURIComponent(terminal.id)}`}
            >
              View its recordings
            </Link>
          }
        >
          <span className="mono">
            {commandLine(terminal.command, terminal.args)}
          </span>{' '}
          is no longer running.
        </EmptyState>
      </FramedState>
    )
  }

  const probe = async (): Promise<ProbeResult> => {
    try {
      const current = await api.getTerminal(id)
      return isLiveState(current.state) ? 'retry' : { ended: 'exited' }
    } catch (e) {
      if (isUnauthorized(e)) {
        auth.sessionEnded()
        return { ended: 'unauthorized' }
      }
      if (isNotFound(e)) return { ended: 'not_found' }
      return 'retry'
    }
  }

  return (
    <>
      <Workspace
        terminalId={terminal.id}
        initialSize={{ rows: terminal.rows, cols: terminal.cols }}
        recordingId={recordingId}
        probe={probe}
        header={
          <>
            {back}
            <h1 className="workspace__command mono">
              {commandLine(terminal.command, terminal.args)}
            </h1>
          </>
        }
        actions={
          <button
            type="button"
            className="btn btn--destructive-text"
            onClick={() => setTerminating(true)}
          >
            <Square aria-hidden size={12} /> Terminate
          </button>
        }
        rail={<GrantsPanel terminalId={terminal.id} compact />}
      />
      {terminating && (
        <TerminateDialog
          terminal={terminal}
          onDone={() => setTerminating(false)}
          onCancel={() => setTerminating(false)}
        />
      )}
    </>
  )
}

function FramedState({
  back,
  children,
}: {
  back: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <div className="framed">
      <div className="spine" data-state="idle" aria-hidden="true">
        <span className="spine__primary" />
        <span className="spine__secondary" />
      </div>
      <main className="framed__main" id="main">
        {back}
        {children}
      </main>
    </div>
  )
}
