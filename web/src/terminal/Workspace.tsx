import { Eye, Smartphone, TriangleAlert } from 'lucide-react'
import { type ReactNode, useEffect, useId, useMemo, useRef, useState } from 'react'

import type { Permissions } from '../api/types'
import { RoleBadge } from '../components/StatusBadge'
import { SignalSpine, type SpineState } from '../components/SignalSpine'
import { shortId } from '../lib/format'
import { type ConnectionState, type ProbeResult, TerminalConnection } from './connection'
import { describeState } from './describeState'
import { useIsPhone, useTerminalEnvironment } from './environmentContext'
import { createFocusEscape, editableHint, readOnlyHint } from './focusEscape'
import type { Participant, RecordingStatusMessage } from './protocol'
import { createScreenQueue } from './screen'
import type { XtermHandle } from './xterm'

export interface WorkspaceProps {
  terminalId: string
  /** Screen size to show before the server reports one, and for anyone who may not resize. */
  initialSize: { rows: number; cols: number }
  /** Owners see recording state; guests never do. */
  recordingId?: string | null
  probe?: () => Promise<ProbeResult>
  header: ReactNode
  actions?: ReactNode
  rail?: ReactNode
  onState?: (state: ConnectionState) => void
}

interface Notice {
  id: number
  tone: 'warning' | 'info'
  text: string
}

const noticeCopy: Record<string, string> = {
  replay_gap: 'Some output was missed while reconnecting. The screen was redrawn from what the server still had.',
  input_overflow: 'Input arrived faster than the terminal could take it. Some keystrokes may be lost.',
  slow_consumer: 'This connection fell behind the terminal output and was restarted.',
}

function spineFor(state: ConnectionState, recording: boolean): SpineState {
  switch (state.kind) {
    case 'connecting':
      return state.attempt ? 'reconnecting' : 'idle'
    case 'live':
      return recording ? 'live-recording' : 'live'
    case 'reconnecting':
      return 'reconnecting'
    case 'ended':
      return state.reason === 'exited' ? 'ended' : 'lost'
  }
}

export function Workspace({ terminalId, initialSize, recordingId, probe, header, actions, rail, onState }: WorkspaceProps) {
  const env = useTerminalEnvironment()
  const phone = useIsPhone()
  const host = useRef<HTMLDivElement>(null)
  const [state, setState] = useState<ConnectionState>({ kind: 'connecting', attempt: 0 })
  const [role, setRole] = useState<Participant['role'] | null>(null)
  const [permissions, setPermissions] = useState<Permissions>({ input: false, resize: false })
  const [participants, setParticipants] = useState<Participant[]>([])
  const [selfId, setSelfId] = useState<string | null>(null)
  const [recording, setRecording] = useState<RecordingStatusMessage | null>(null)
  const [notices, setNotices] = useState<Notice[]>([])
  const phoneRef = useRef(phone)
  const probeRef = useRef(probe)
  const onStateRef = useRef(onState)
  const connectionRef = useRef<TerminalConnection | null>(null)
  const xtermRef = useRef<XtermHandle | null>(null)
  const editableRef = useRef(false)
  const hintId = useId()

  useEffect(() => {
    phoneRef.current = phone
    probeRef.current = probe
    onStateRef.current = onState
  })

  useEffect(() => {
    if (!host.current) return
    const xterm = env.createXterm(host.current, { label: 'Terminal input' })
    xtermRef.current = xterm
    const setEditable = (editable: boolean) => {
      editableRef.current = editable
      xterm.setInputEnabled(editable)
    }
    setEditable(false)
    xterm.setKeyHandler(createFocusEscape(() => editableRef.current))
    xterm.describeBy(hintId)
    const screen = createScreenQueue(xterm, host.current)
    screen.resize(initialSize.cols, initialSize.rows)
    let noticeId = 0
    const notify = (tone: Notice['tone'], text: string) =>
      setNotices((list) => [...list.slice(-2), { id: ++noticeId, tone, text }])

    let serverSize = initialSize
    const canResize = () => connection.permissions.resize && !phoneRef.current
    const fitAndSend = () => {
      if (!canResize()) {
        screen.resize(serverSize.cols, serverSize.rows)
        return
      }
      const size = xterm.proposeFit()
      if (!size) return
      screen.resize(size.cols, size.rows)
      connection.sendResize(size.rows, size.cols)
    }

    const connection = new TerminalConnection({
      terminalId,
      origin: env.origin,
      socketFactory: env.socketFactory,
      probe: () => (probeRef.current ? probeRef.current() : Promise.resolve('retry' as const)),
      handlers: {
        onState: (next) => {
          setState(next)
          onStateRef.current?.(next)
          if (next.kind !== 'live') setEditable(false)
        },
        onReady: (message, fresh) => {
          serverSize = { rows: message.session.rows, cols: message.session.cols }
          // A fresh stream redraws from scratch; output from the old one must not land after it.
          if (fresh) screen.reset(serverSize.cols, serverSize.rows)
          setRole(message.role)
          fitAndSend()
        },
        onOutput: (data) => screen.write(data),
        onResize: (rows, cols) => {
          serverSize = { rows, cols }
          if (!canResize()) screen.resize(cols, rows)
        },
        onPermissions: (next) => {
          setPermissions(next)
          setEditable(next.input && !phoneRef.current)
        },
        onPresence: (list, self) => {
          setParticipants(list)
          setSelfId(self)
        },
        onRecordingStatus: (message) => {
          setRecording(message)
          if (message.status !== 'recording') notify('warning', message.message)
        },
        onNotice: (code, message) => notify(code === 'replay_gap' ? 'info' : 'warning', noticeCopy[code] ?? message.message),
      },
    })
    connectionRef.current = connection
    const stopInput = xterm.onData((data) => {
      if (!phoneRef.current) connection.sendInput(data)
    })

    let frame = 0
    const observer =
      typeof ResizeObserver === 'function'
        ? new ResizeObserver(() => {
            cancelAnimationFrame(frame)
            frame = requestAnimationFrame(() => connection.state.kind === 'live' && fitAndSend())
          })
        : null
    observer?.observe(host.current)

    connection.start()
    return () => {
      observer?.disconnect()
      cancelAnimationFrame(frame)
      stopInput()
      connection.stop()
      screen.dispose()
      xterm.dispose()
      connectionRef.current = null
      xtermRef.current = null
    }
    // The connection lives for the terminal; size changes are handled inside.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [terminalId, env])

  useEffect(() => {
    const xterm = xtermRef.current
    const connection = connectionRef.current
    if (!xterm || !connection) return
    editableRef.current = connection.permissions.input && connection.state.kind === 'live' && !phone
    xterm.setInputEnabled(editableRef.current)
  }, [phone])

  const knownRecording = recording ? recording.status === 'recording' : !!recordingId
  const isRecording = recordingId !== undefined && knownRecording && state.kind !== 'ended'
  const canType = permissions.input && state.kind === 'live'
  const ended = state.kind === 'ended'

  const sortedParticipants = useMemo(() => {
    const order = { owner: 0, editor: 1, viewer: 2 }
    return [...participants].sort((a, b) => order[a.role] - order[b.role] || Number(b.id === selfId) - Number(a.id === selfId))
  }, [participants, selfId])

  return (
    <main className="workspace" id="main">
      <SignalSpine state={spineFor(state, isRecording)} />
      <header className="workspace__bar">
        <div className="workspace__title">{header}</div>
        <p className="readout" data-testid="readout" role="status" aria-live="polite">
          <span className="readout__state">{describeState(state)}</span>
          {isRecording && <span className="readout__recording">Recording</span>}
        </p>
        {actions && <div className="workspace__actions">{actions}</div>}
      </header>

      <section className="workspace__terminal" aria-label="Terminal">
        {state.kind === 'live' && role === 'viewer' && (
          <p className="terminal-note">
            <Eye aria-hidden size={14} /> You are viewing. Only the owner and the editor can type.
          </p>
        )}
        {state.kind === 'live' && role !== 'viewer' && phone && (
          <p className="terminal-note">
            <Smartphone aria-hidden size={14} /> Typing is off on phone-sized screens. You can still watch, share, and end this terminal.
          </p>
        )}
        <div ref={host} className="workspace__screen" data-input={canType && !phone ? 'on' : 'off'} data-ended={ended || undefined} />
        <p id={hintId} className="terminal-hint">
          {canType && !phone ? editableHint : readOnlyHint}
        </p>
      </section>

      <aside className="workspace__rail" aria-label="Terminal details">
        <div className="notices" aria-live="polite">
          {notices.map((notice) => (
            <p key={notice.id} className={`notice notice--${notice.tone}`}>
              <TriangleAlert aria-hidden size={14} />
              <span>{notice.text}</span>
            </p>
          ))}
        </div>
        <section className="rail-section" aria-labelledby="people-heading">
          <h2 id="people-heading">People</h2>
          {sortedParticipants.length === 0 ? (
            <p className="muted">{state.kind === 'live' ? 'Waiting for the participant list.' : 'Nobody is connected.'}</p>
          ) : (
            <ul className="people" aria-label="People in this terminal">
              {sortedParticipants.map((p) => (
                <li key={p.id} className="people__item">
                  <span className="people__name">
                    {p.id === selfId ? (
                      'You'
                    ) : p.role === 'owner' ? (
                      role === 'owner' ? 'You, in another tab or device' : 'Administrator'
                    ) : (
                      <>
                        Guest <span className="mono">{shortId(p.id)}</span>
                      </>
                    )}
                  </span>
                  <RoleBadge role={p.role} />
                </li>
              ))}
            </ul>
          )}
        </section>
        {rail}
      </aside>
    </main>
  )
}
