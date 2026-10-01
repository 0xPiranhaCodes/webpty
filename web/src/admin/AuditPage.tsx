import { RefreshCw } from 'lucide-react'
import { useState } from 'react'

import { useApi } from '../api/services'
import type { AuditEvent } from '../api/types'
import { describeError } from '../components/describeError'
import { EmptyState, ErrorState, InlineError, Loading } from '../components/StateViews'
import { formatDateTime } from '../lib/format'
import { toApiError, useAsync } from '../lib/useAsync'

function Details({ details }: { details: Record<string, string> }) {
  const entries = Object.entries(details)
  if (entries.length === 0) return <span className="muted">—</span>
  return (
    <ul className="kv">
      {entries.map(([k, v]) => (
        <li key={k} className="mono">{`${k}=${v}`}</li>
      ))}
    </ul>
  )
}

export function AuditPage() {
  const api = useApi()
  const first = useAsync('audit', (signal) => api.auditPage(undefined, signal))
  const [older, setOlder] = useState<{ events: AuditEvent[]; next: string | null } | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const events = [...(first.data?.events ?? []), ...(older?.events ?? [])]
  const next = older ? older.next : (first.data?.nextCursor ?? null)

  const refresh = () => {
    setOlder(null)
    setError(null)
    first.reload()
  }

  const loadMore = async () => {
    if (!next) return
    setPending(true)
    setError(null)
    try {
      const page = await api.auditPage(next)
      setOlder((prev) => ({ events: [...(prev?.events ?? []), ...page.events], next: page.nextCursor }))
    } catch (e) {
      const copy = describeError(toApiError(e))
      setError(`${copy.title} ${copy.body}`)
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="page">
      <header className="page__header">
        <h1>Audit log</h1>
        <button type="button" className="btn" onClick={refresh} disabled={first.status === 'loading' || pending}>
          <RefreshCw aria-hidden size={14} /> Refresh
        </button>
      </header>
      <p className="lede muted">Security-relevant events, newest first. Tokens and passwords are never recorded.</p>
      {first.status === 'loading' && !first.data && <Loading label="Loading audit events" />}
      {first.status === 'error' && <ErrorState error={first.error} onRetry={first.reload} />}
      {first.data &&
        (events.length === 0 ? (
          <EmptyState title="No audit events yet" />
        ) : (
          <>
            <div className="table-wrap">
              <table className="table">
                <caption className="visually-hidden">Audit events</caption>
                <thead>
                  <tr>
                    <th scope="col">Time</th>
                    <th scope="col">Event</th>
                    <th scope="col">Address</th>
                    <th scope="col">Details</th>
                  </tr>
                </thead>
                <tbody>
                  {events.map((event) => (
                    <tr key={event.id}>
                      <td className="nowrap">{formatDateTime(event.occurredAt)}</td>
                      <td className="mono">{event.type}</td>
                      <td className="mono">{event.remoteAddr || '—'}</td>
                      <td>
                        <Details details={event.details} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {error && <InlineError>{error}</InlineError>}
            <div className="pager">
              {next ? (
                <button type="button" className="btn" onClick={loadMore} disabled={pending}>
                  {pending ? 'Loading…' : 'Load older events'}
                </button>
              ) : (
                <p className="muted">You have reached the oldest event.</p>
              )}
            </div>
          </>
        ))}
    </div>
  )
}
