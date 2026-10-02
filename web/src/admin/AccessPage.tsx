import { useId, useState } from 'react'
import { Link } from 'react-router'

import { useApi } from '../api/services'
import { EmptyState, ErrorState, Loading } from '../components/StateViews'
import { commandLine } from '../lib/format'
import { useAsync } from '../lib/useAsync'
import { GrantsPanel } from './GrantsPanel'

export function AccessPage() {
  const api = useApi()
  const id = useId()
  const terminals = useAsync('access:terminals', (signal) =>
    api.listTerminals(signal),
  )
  const [chosen, setChosen] = useState<string | null>(null)
  const running = (terminals.data ?? []).filter((t) => t.state === 'running')
  const selected = running.find((t) => t.id === chosen) ?? running[0]

  return (
    <div className="page">
      <header className="page__header">
        <h1>Access grants</h1>
      </header>
      <p className="lede muted">
        Share links let someone watch or type in one running terminal. Links end
        when the terminal ends.
      </p>
      {terminals.status === 'loading' && !terminals.data && (
        <Loading label="Loading terminals" />
      )}
      {terminals.status === 'error' && (
        <ErrorState error={terminals.error} onRetry={terminals.reload} />
      )}
      {terminals.data &&
        (running.length === 0 ? (
          <EmptyState
            title="No running terminals"
            action={
              <Link className="btn" to="/admin/sessions">
                Go to Live sessions
              </Link>
            }
          >
            Links belong to a running terminal. Start one first.
          </EmptyState>
        ) : (
          <>
            <div className="field field--select">
              <label htmlFor={id}>Terminal</label>
              <select
                id={id}
                className="input mono"
                value={selected.id}
                onChange={(e) => setChosen(e.target.value)}
              >
                {running.map((t) => (
                  <option key={t.id} value={t.id}>
                    {commandLine(t.command, t.args)} ({t.id})
                  </option>
                ))}
              </select>
            </div>
            <GrantsPanel key={selected.id} terminalId={selected.id} />
          </>
        ))}
    </div>
  )
}
