import { Info, TriangleAlert } from 'lucide-react'
import type { ReactNode } from 'react'
import { Link } from 'react-router'

import { useApi } from '../api/services'
import type { Recording, RuntimeSettings, Terminal } from '../api/types'
import { ErrorState, Loading } from '../components/StateViews'
import {
  formatBytes,
  formatDateTime,
  formatGoDuration,
  plural,
} from '../lib/format'
import { useAsync } from '../lib/useAsync'
import { findings, setting } from './findings'

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="ledger__row">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  )
}

export function OverviewPage() {
  const api = useApi()
  const data = useAsync('overview', async (signal) => {
    const [terminals, recordings, settings] = await Promise.all([
      api.listTerminals(signal),
      api.listRecordings({ limit: 500 }, signal),
      api.settings(signal),
    ])
    return { terminals, recordings, settings }
  })

  return (
    <div className="page">
      <header className="page__header">
        <h1>Overview</h1>
      </header>
      {data.status === 'loading' && !data.data && (
        <Loading label="Loading overview" />
      )}
      {data.status === 'error' && (
        <ErrorState error={data.error} onRetry={data.reload} />
      )}
      {data.data && <Ledger {...data.data} />}
    </div>
  )
}

function Ledger({
  terminals,
  recordings,
  settings,
}: {
  terminals: Terminal[]
  recordings: Recording[]
  settings: RuntimeSettings
}) {
  const running = terminals.filter((t) => t.state === 'running')
  const ended = terminals.length - running.length
  const participants = running.reduce((sum, t) => sum + t.participants, 0)
  const stored = recordings.filter((r) => r.status !== 'deleted')
  const bytes = stored.reduce((sum, r) => sum + r.compressedBytes, 0)
  const active = stored.filter((r) => r.status === 'recording').length
  const nextExpiry = stored
    .map((r) => r.retainUntil)
    .filter((d): d is string => !!d)
    .sort()[0]
  const retention = setting(settings, 'WEBPTY_RECORDING_RETENTION')
  const list = findings(terminals, recordings, settings)
  const capped = recordings.length >= 500 ? 'at least ' : ''

  return (
    <>
      <dl className="ledger">
        <Row label="Terminals">
          {plural(running.length, 'terminal')} running, {ended} ended.{' '}
          <Link to="/admin/sessions">Live sessions</Link>
        </Row>
        <Row label="People connected">
          {participants === 0
            ? 'Nobody is connected right now.'
            : `${plural(participants, 'participant')} across ${plural(running.filter((t) => t.participants > 0).length, 'terminal')}.`}
        </Row>
        <Row label="Recordings">
          {capped}
          {plural(stored.length, 'recording')} stored, {formatBytes(bytes)}{' '}
          compressed
          {active ? `, ${active} in progress` : ''}.
        </Row>
        <Row label="Retention">
          {retention
            ? `Kept for ${formatGoDuration(retention)} after a terminal ends.`
            : 'Unknown.'}
          {nextExpiry ? ` Next removal ${formatDateTime(nextExpiry)}.` : ''}
        </Row>
      </dl>

      <section className="section" aria-labelledby="findings">
        <h2 id="findings">Setup and security</h2>
        {list.length === 0 ? (
          <p className="muted">
            No problems found in the current configuration.
          </p>
        ) : (
          <ul className="findings">
            {list.map((finding, index) => (
              <li key={index} className={`finding finding--${finding.tone}`}>
                {finding.tone === 'warning' ? (
                  <TriangleAlert aria-hidden size={16} />
                ) : (
                  <Info aria-hidden size={16} />
                )}
                <span className="visually-hidden">
                  {finding.tone === 'warning' ? 'Warning: ' : 'Note: '}
                </span>
                <span>{finding.text}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </>
  )
}
