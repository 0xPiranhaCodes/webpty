import { ArrowLeft, Download, Pause, Play, Trash2, TriangleAlert } from 'lucide-react'
import { type KeyboardEvent, useEffect, useId, useReducer, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'

import { useApi } from '../api/services'
import type { PlaybackEvent, Recording } from '../api/types'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { SignalSpine } from '../components/SignalSpine'
import { ErrorState, Loading } from '../components/StateViews'
import { RecordingStatusBadge } from '../components/StatusBadge'
import { formatBytes, formatClock, formatDateTime } from '../lib/format'
import { useAsync } from '../lib/useAsync'
import { useTerminalEnvironment } from '../terminal/environmentContext'
import { createFocusEscape } from '../terminal/focusEscape'
import { createScreenQueue } from '../terminal/screen'
import { loadAllEvents, Player, type Speed, speeds } from './player'

const failureCopy: Record<string, string> = {
  queue_overflow: 'output arrived faster than it could be stored',
  storage_error: 'the database could not store it',
  encoding_error: 'some output could not be encoded',
  size_limit: 'it reached WEBPTY_RECORDING_MAX_BYTES',
  shutdown_timeout: 'webpty shut down before it finished writing',
  interrupted: 'webpty stopped unexpectedly while it was recording',
  corrupt: 'stored data failed validation',
}

const seekStepMs = 5_000

export function RecordingPlayerPage() {
  const { id = '' } = useParams()
  const api = useApi()
  const [loaded, setLoaded] = useState(0)
  const data = useAsync(`recording:${id}`, async (signal) => {
    const recording = await api.getRecording(id, signal)
    const events = await loadAllEvents((cursor) => api.recordingEvents(id, cursor, signal), setLoaded)
    return { recording, events }
  })

  const back = (
    <Link to="/admin/recordings" className="back-link">
      <ArrowLeft aria-hidden size={14} /> Recordings
    </Link>
  )

  if (!data.data) {
    return (
      <main className="workspace workspace--playback" id="main">
        <SignalSpine state={data.status === 'error' ? 'lost' : 'idle'} />
        <header className="workspace__bar">
          <div className="workspace__title">{back}</div>
        </header>
        <section className="workspace__terminal workspace__terminal--message" aria-label="Recording playback">
          {data.status === 'error' ? (
            <ErrorState error={data.error} subject="recording" onRetry={data.error.code === 'offline' ? data.reload : undefined} />
          ) : (
            <Loading label={loaded ? `Loading recording, ${loaded} events` : 'Loading recording'} />
          )}
        </section>
      </main>
    )
  }
  return <PlayerView recording={data.data.recording} events={data.data.events} back={back} />
}

function PlayerView({ recording, events, back }: { recording: Recording; events: PlaybackEvent[]; back: React.ReactNode }) {
  const api = useApi()
  const env = useTerminalEnvironment()
  const navigate = useNavigate()
  const host = useRef<HTMLDivElement>(null)
  const playerRef = useRef<Player | null>(null)
  const [, rerender] = useReducer((n: number) => n + 1, 0)
  const [deleting, setDeleting] = useState(false)
  const speedName = useId()

  useEffect(() => {
    if (!host.current) return
    const xterm = env.createXterm(host.current, { label: 'Recorded terminal' })
    xterm.setInputEnabled(false)
    // Nothing can be typed here, so the screen stays out of the Tab order.
    xterm.setTabStop(false)
    xterm.setKeyHandler(createFocusEscape(() => false))
    const screen = createScreenQueue(xterm, host.current)
    const player = new Player({
      events,
      durationMs: recording.durationMs,
      rows: recording.rows,
      cols: recording.cols,
      sink: screen,
      onChange: rerender,
    })
    playerRef.current = player
    rerender()
    return () => {
      player.dispose()
      screen.dispose()
      xterm.dispose()
      playerRef.current = null
    }
  }, [env, events, recording])

  const player = playerRef.current
  const position = player?.positionMs ?? 0
  const duration = player?.durationMs ?? recording.durationMs
  const playing = player?.playing ?? false
  const incomplete = recording.status === 'incomplete'

  const onSeekKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (!player) return
    const targets: Record<string, number> = {
      Home: 0,
      End: duration,
      ArrowLeft: position - seekStepMs,
      ArrowDown: position - seekStepMs,
      ArrowRight: position + seekStepMs,
      ArrowUp: position + seekStepMs,
      PageDown: position - duration / 10,
      PageUp: position + duration / 10,
    }
    if (!(event.key in targets)) return
    event.preventDefault()
    player.requestSeek(targets[event.key])
  }

  return (
    <main className="workspace workspace--playback" id="main">
      <SignalSpine state="playback" />
      <header className="workspace__bar">
        <div className="workspace__title">
          {back}
          <h1 className="workspace__command">
            Recording <span className="mono">{recording.id}</span>
          </h1>
        </div>
        <p className="readout" data-testid="readout" role="status" aria-live="polite">
          <span className="readout__state readout__state--playback">Playback</span>
          <span className="muted">{playing ? 'playing' : 'paused'}</span>
        </p>
        <div className="workspace__actions">
          <a className="btn" href={api.exportUrl(recording.id)} download>
            <Download aria-hidden size={14} /> Export as asciicast
          </a>
          <button type="button" className="btn btn--destructive-text" onClick={() => setDeleting(true)}>
            <Trash2 aria-hidden size={14} /> Delete recording
          </button>
        </div>
      </header>

      <section className="workspace__terminal" aria-label="Recording playback">
        <div ref={host} className="workspace__screen" data-input="off" />
        <div className="transport">
          <button type="button" className="btn btn--primary transport__play" onClick={() => player?.toggle()} aria-label={playing ? 'Pause' : 'Play'}>
            {playing ? <Pause aria-hidden size={14} /> : <Play aria-hidden size={14} />}
          </button>
          <input
            type="range"
            className="transport__seek"
            aria-label="Position"
            min={0}
            max={duration}
            step={100}
            value={position}
            aria-valuetext={`${formatClock(position)} of ${formatClock(duration)}`}
            onChange={(e) => player?.requestSeek(Number(e.target.value))}
            onKeyDown={onSeekKey}
          />
          <span className="transport__time mono">{`${formatClock(position)} / ${formatClock(duration)}`}</span>
          <div className="speed" role="radiogroup" aria-label="Playback speed">
            {speeds.map((s) => (
              <label key={s} className="speed__option">
                <input type="radio" name={speedName} checked={(player?.speed ?? 1) === s} onChange={() => player?.setSpeed(s as Speed)} />
                <span>{`${s}×`}</span>
              </label>
            ))}
          </div>
        </div>
      </section>

      <aside className="workspace__rail" aria-label="Recording details">
        {incomplete && (
          <p className="notice notice--warning">
            <TriangleAlert aria-hidden size={14} />
            <span>
              This recording is incomplete: {failureCopy[recording.failureCode ?? ''] ?? 'it stopped before the terminal ended'}. Playback ends where the
              recording stopped.
            </span>
          </p>
        )}
        <section className="rail-section" aria-labelledby="rec-details">
          <h2 id="rec-details">Details</h2>
          <dl className="facts">
            <dt>Status</dt>
            <dd>
              <RecordingStatusBadge recording={recording} />
            </dd>
            <dt>Terminal</dt>
            <dd className="mono">{recording.terminalId}</dd>
            <dt>Started</dt>
            <dd>{formatDateTime(recording.startedAt)}</dd>
            <dt>Length</dt>
            <dd className="mono">{formatClock(recording.durationMs)}</dd>
            <dt>Size</dt>
            <dd>
              {formatBytes(recording.compressedBytes)} compressed, {formatBytes(recording.uncompressedBytes)} raw
            </dd>
            <dt>Screen</dt>
            <dd className="mono">
              {recording.cols}×{recording.rows}
            </dd>
            <dt>Kept until</dt>
            <dd>{formatDateTime(recording.retainUntil)}</dd>
          </dl>
        </section>
      </aside>

      {deleting && (
        <ConfirmDialog
          title="Delete this recording?"
          confirmLabel="Delete"
          onCancel={() => setDeleting(false)}
          onConfirm={async () => {
            await api.deleteRecording(recording.id)
            navigate('/admin/recordings', { state: { notice: 'Recording deleted.' } })
          }}
        >
          <p>Its events are removed permanently. The audit log keeps a record that it existed.</p>
        </ConfirmDialog>
      )}
    </main>
  )
}
