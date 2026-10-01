import { Plus, Square } from 'lucide-react'
import { type FormEvent, useId, useState } from 'react'
import { Link, useNavigate } from 'react-router'

import { useApi } from '../api/services'
import type { Terminal } from '../api/types'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { describeError } from '../components/describeError'
import { EmptyState, ErrorState, InlineError, Loading } from '../components/StateViews'
import { TerminalStateBadge } from '../components/StatusBadge'
import { commandLine, formatDateTime, formatTime } from '../lib/format'
import { toApiError, useAsync } from '../lib/useAsync'

const isLive = (t: Terminal) => t.state === 'running' || t.state === 'starting'

export function SessionsPage() {
  const api = useApi()
  const terminals = useAsync('terminals', (signal) => api.listTerminals(signal))
  const [creating, setCreating] = useState(false)
  const [terminating, setTerminating] = useState<Terminal | null>(null)

  const sorted = terminals.data ? [...terminals.data].sort((a, b) => Number(isLive(b)) - Number(isLive(a)) || b.createdAt.localeCompare(a.createdAt)) : []

  return (
    <div className="page">
      <header className="page__header">
        <h1>Live sessions</h1>
        {!creating && (
          <button type="button" className="btn btn--primary" onClick={() => setCreating(true)}>
            <Plus aria-hidden size={14} /> New terminal
          </button>
        )}
      </header>

      {creating && <CreateTerminalForm onCancel={() => setCreating(false)} />}

      {terminals.status === 'loading' && !terminals.data && <Loading label="Loading terminals" />}
      {terminals.status === 'error' && <ErrorState error={terminals.error} onRetry={terminals.reload} />}
      {terminals.data &&
        (sorted.length === 0 ? (
          <EmptyState title="No terminals yet">Start one with New terminal. It runs on this machine as the user running webpty.</EmptyState>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <caption className="visually-hidden">Terminals</caption>
              <thead>
                <tr>
                  <th scope="col">Command</th>
                  <th scope="col">State</th>
                  <th scope="col" className="num">People</th>
                  <th scope="col">Size</th>
                  <th scope="col">Started</th>
                  <th scope="col">Last activity</th>
                  <th scope="col">
                    <span className="visually-hidden">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {sorted.map((t) => {
                  const cmd = commandLine(t.command, t.args)
                  return (
                    <tr key={t.id} data-live={isLive(t) || undefined}>
                      <td>
                        {isLive(t) ? (
                          <Link className="mono cell-link" to={`/admin/sessions/${encodeURIComponent(t.id)}`}>
                            {cmd}
                          </Link>
                        ) : (
                          <span className="mono">{cmd}</span>
                        )}
                        <span className="cell-sub mono">{t.id}</span>
                      </td>
                      <td>
                        <TerminalStateBadge terminal={t} />
                        {t.failure && <span className="cell-sub">{t.failure}</span>}
                      </td>
                      <td className="num">{isLive(t) ? t.participants : '—'}</td>
                      <td className="mono">
                        {t.cols}×{t.rows}
                      </td>
                      <td>{formatDateTime(t.startedAt ?? t.createdAt)}</td>
                      <td>{formatTime(t.endedAt ?? t.lastActivityAt)}</td>
                      <td className="actions">
                        {isLive(t) ? (
                          <button type="button" className="btn btn--quiet btn--destructive-text" onClick={() => setTerminating(t)} aria-label={`Terminate ${cmd}`}>
                            <Square aria-hidden size={12} /> Terminate
                          </button>
                        ) : (
                          <Link className="btn btn--quiet" to={`/admin/recordings?terminal=${encodeURIComponent(t.id)}`}>
                            Recordings
                          </Link>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        ))}

      {terminating && (
        <TerminateDialog
          terminal={terminating}
          onDone={() => {
            setTerminating(null)
            terminals.reload()
          }}
          onCancel={() => setTerminating(null)}
        />
      )}
    </div>
  )
}

export function TerminateDialog({ terminal, onDone, onCancel }: { terminal: Terminal; onDone: () => void; onCancel: () => void }) {
  const api = useApi()
  return (
    <ConfirmDialog
      title="Terminate this terminal?"
      confirmLabel="Terminate"
      onCancel={onCancel}
      onConfirm={async () => {
        await api.terminateTerminal(terminal.id)
        onDone()
      }}
    >
      <p>
        <span className="mono">{commandLine(terminal.command, terminal.args)}</span> is stopped and everyone connected is disconnected. Its recording, if any, is kept.
      </p>
    </ConfirmDialog>
  )
}

function parseSize(value: string): number | undefined {
  const n = Number(value)
  return value.trim() === '' || !Number.isInteger(n) ? undefined : n
}

function CreateTerminalForm({ onCancel }: { onCancel: () => void }) {
  const api = useApi()
  const navigate = useNavigate()
  const id = useId()
  const [command, setCommand] = useState('')
  const [args, setArgs] = useState('')
  const [rows, setRows] = useState('')
  const [cols, setCols] = useState('')
  const [record, setRecord] = useState(true)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setPending(true)
    setError(null)
    try {
      const terminal = await api.createTerminal({
        command: command.trim(),
        args: args.split('\n').filter((a) => a !== ''),
        rows: parseSize(rows),
        cols: parseSize(cols),
        record,
      })
      navigate(`/admin/sessions/${encodeURIComponent(terminal.id)}`)
    } catch (e) {
      const copy = describeError(toApiError(e), 'terminal')
      setError(`${copy.title} ${copy.body}`)
      setPending(false)
    }
  }

  return (
    <form className="panel form create-form" onSubmit={submit} aria-labelledby={`${id}-h`}>
      <h2 id={`${id}-h`}>New terminal</h2>
      <div className="field">
        <label htmlFor={`${id}-cmd`}>Command</label>
        <input id={`${id}-cmd`} className="input mono" value={command} onChange={(e) => setCommand(e.target.value)} placeholder="Server default" spellCheck={false} autoCapitalize="off" />
        <p className="field-hint">Leave empty to use the command webpty was started with.</p>
      </div>
      <div className="field">
        <label htmlFor={`${id}-args`}>Arguments</label>
        <textarea id={`${id}-args`} className="input mono" rows={2} value={args} onChange={(e) => setArgs(e.target.value)} spellCheck={false} autoCapitalize="off" />
        <p className="field-hint">One argument per line, passed exactly as written. No shell expansion.</p>
      </div>
      <div className="field-row">
        <div className="field field--narrow">
          <label htmlFor={`${id}-rows`}>Rows</label>
          <input id={`${id}-rows`} className="input mono" inputMode="numeric" value={rows} onChange={(e) => setRows(e.target.value)} placeholder="24" />
        </div>
        <div className="field field--narrow">
          <label htmlFor={`${id}-cols`}>Columns</label>
          <input id={`${id}-cols`} className="input mono" inputMode="numeric" value={cols} onChange={(e) => setCols(e.target.value)} placeholder="80" />
        </div>
        <label className="check">
          <input type="checkbox" checked={record} onChange={(e) => setRecord(e.target.checked)} />
          <span>Record this terminal</span>
        </label>
      </div>
      {error && <InlineError>{error}</InlineError>}
      <div className="form__actions">
        <button type="submit" className="btn btn--primary" disabled={pending}>
          {pending ? 'Starting…' : 'Start terminal'}
        </button>
        <button type="button" className="btn" onClick={onCancel} disabled={pending}>
          Cancel
        </button>
      </div>
    </form>
  )
}
