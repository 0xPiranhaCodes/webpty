import { Trash2 } from 'lucide-react'
import { useState } from 'react'
import { Link, useLocation, useSearchParams } from 'react-router'

import { useApi } from '../api/services'
import { ConfirmDialog } from '../components/ConfirmDialog'
import type { Recording } from '../api/types'
import { describeError } from '../components/describeError'
import {
  EmptyState,
  ErrorState,
  InlineError,
  Loading,
} from '../components/StateViews'
import { RecordingStatusBadge } from '../components/StatusBadge'
import { formatBytes, formatClock, formatDateTime, plural } from '../lib/format'
import { toApiError, useAsync } from '../lib/useAsync'

export function RecordingsPage() {
  const api = useApi()
  const [params] = useSearchParams()
  const location = useLocation()
  const terminalId = params.get('terminal') ?? undefined
  const first = useAsync(`recordings:${terminalId ?? ''}`, (signal) =>
    api.recordingsPage({ terminalId }, signal),
  )
  const [older, setOlder] = useState<{
    key: string
    recordings: Recording[]
    next: string | null
  } | null>(null)
  const [pending, setPending] = useState(false)
  const [loadError, setLoadError] = useState<string | null>(null)
  const listKey = terminalId ?? ''
  // Older pages belong to the first page they were loaded after; a reload or a new filter drops them.
  const olderPages =
    older && older.key === listKey && first.status === 'ready' ? older : null
  const rows = first.data
    ? [...first.data.recordings, ...(olderPages?.recordings ?? [])]
    : undefined
  const next = olderPages ? olderPages.next : (first.data?.nextCursor ?? null)

  const reload = () => {
    setOlder(null)
    setLoadError(null)
    first.reload()
  }

  const loadOlder = async () => {
    if (!next) return
    setPending(true)
    setLoadError(null)
    try {
      const page = await api.recordingsPage({ terminalId, before: next })
      setOlder({
        key: listKey,
        recordings: [...(olderPages?.recordings ?? []), ...page.recordings],
        next: page.nextCursor,
      })
    } catch (e) {
      const copy = describeError(toApiError(e))
      setLoadError(`${copy.title} ${copy.body}`)
    } finally {
      setPending(false)
    }
  }
  const [confirmRetention, setConfirmRetention] = useState(false)
  const [notice, setNotice] = useState<string | null>(
    (location.state as { notice?: string } | null)?.notice ?? null,
  )

  return (
    <div className="page">
      <header className="page__header">
        <h1>Recordings</h1>
        <button
          type="button"
          className="btn"
          onClick={() => setConfirmRetention(true)}
        >
          <Trash2 aria-hidden size={14} /> Delete expired recordings
        </button>
      </header>
      <p className="notice-line" role="status" aria-live="polite">
        {notice}
      </p>
      {terminalId && (
        <p className="filter-line">
          Showing recordings of terminal{' '}
          <span className="mono">{terminalId}</span>.{' '}
          <Link to="/admin/recordings">Show all</Link>
        </p>
      )}
      {first.status === 'loading' && !first.data && (
        <Loading label="Loading recordings" />
      )}
      {first.status === 'error' && (
        <ErrorState error={first.error} onRetry={reload} />
      )}
      {rows &&
        (rows.length === 0 ? (
          <EmptyState title="No recordings">
            Terminals are recorded unless recording was turned off when they
            started.
          </EmptyState>
        ) : (
          <>
            <div className="table-wrap">
              <table className="table">
                <caption className="visually-hidden">Recordings</caption>
                <thead>
                  <tr>
                    <th scope="col">Started</th>
                    <th scope="col">Status</th>
                    <th scope="col" className="num">
                      Length
                    </th>
                    <th scope="col">Terminal</th>
                    <th scope="col" className="num">
                      Size
                    </th>
                    <th scope="col">Kept until</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <tr key={r.id}>
                      <td>
                        {r.status === 'deleted' ? (
                          formatDateTime(r.startedAt)
                        ) : (
                          <Link
                            className="cell-link"
                            to={`/admin/recordings/${encodeURIComponent(r.id)}`}
                            aria-label={`Play recording from ${formatDateTime(r.startedAt)}`}
                          >
                            {formatDateTime(r.startedAt)}
                          </Link>
                        )}
                      </td>
                      <td>
                        <RecordingStatusBadge recording={r} />
                      </td>
                      <td className="num mono">{formatClock(r.durationMs)}</td>
                      <td className="mono">{r.terminalId}</td>
                      <td className="num">{formatBytes(r.compressedBytes)}</td>
                      <td>{formatDateTime(r.retainUntil)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {loadError && <InlineError>{loadError}</InlineError>}
            {next && (
              <div className="pager">
                <button
                  type="button"
                  className="btn"
                  onClick={loadOlder}
                  disabled={pending}
                >
                  {pending ? 'Loading…' : 'Load older recordings'}
                </button>
              </div>
            )}
          </>
        ))}

      {confirmRetention && (
        <ConfirmDialog
          title="Delete expired recordings now?"
          confirmLabel="Delete expired"
          onCancel={() => setConfirmRetention(false)}
          onConfirm={async () => {
            const deleted = await api.runRetention()
            setConfirmRetention(false)
            setNotice(
              deleted === 0
                ? 'No recordings had expired.'
                : `Deleted ${plural(deleted, 'expired recording')}.`,
            )
            reload()
          }}
        >
          <p>
            Recordings past their retention date are deleted permanently. This
            also runs automatically every hour.
          </p>
        </ConfirmDialog>
      )}
    </div>
  )
}
