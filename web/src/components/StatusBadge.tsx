import {
  Circle,
  CircleCheck,
  CircleDashed,
  CircleX,
  Disc,
  Square,
  TriangleAlert,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'

import type { Grant, Recording, Terminal } from '../api/types'

type Tone =
  | 'live'
  | 'recorded'
  | 'warning'
  | 'destructive'
  | 'neutral'
  | 'muted'

function Badge({
  tone,
  icon: Icon,
  children,
}: {
  tone: Tone
  icon: LucideIcon
  children: string
}) {
  return (
    <span className={`badge badge--${tone}`}>
      <Icon
        aria-hidden
        size={12}
        strokeWidth={2.25}
        fill={Icon === Circle || Icon === Disc ? 'currentColor' : 'none'}
      />
      {children}
    </span>
  )
}

export function TerminalStateBadge({
  terminal,
}: {
  terminal: Pick<Terminal, 'state' | 'exitCode' | 'exitSignal'>
}) {
  switch (terminal.state) {
    case 'running':
      return (
        <Badge tone="live" icon={Circle}>
          Running
        </Badge>
      )
    case 'starting':
      return (
        <Badge tone="neutral" icon={CircleDashed}>
          Starting
        </Badge>
      )
    case 'failed':
      return (
        <Badge tone="destructive" icon={CircleX}>
          Failed to start
        </Badge>
      )
    case 'terminated':
      return (
        <Badge tone="muted" icon={Square}>
          Terminated
        </Badge>
      )
    case 'exited':
      return (
        <Badge tone="muted" icon={Square}>
          {terminal.exitSignal
            ? `Exited, ${terminal.exitSignal}`
            : `Exited ${terminal.exitCode ?? ''}`.trim()}
        </Badge>
      )
  }
}

export function RecordingStatusBadge({
  recording,
}: {
  recording: Pick<Recording, 'status'>
}) {
  switch (recording.status) {
    case 'recording':
      return (
        <Badge tone="recorded" icon={Disc}>
          Recording
        </Badge>
      )
    case 'complete':
      return (
        <Badge tone="neutral" icon={CircleCheck}>
          Complete
        </Badge>
      )
    case 'incomplete':
      return (
        <Badge tone="warning" icon={TriangleAlert}>
          Incomplete
        </Badge>
      )
    case 'deleted':
      return (
        <Badge tone="muted" icon={CircleX}>
          Deleted
        </Badge>
      )
  }
}

const grantCopy: Record<Grant['status'], [Tone, LucideIcon, string]> = {
  active: ['neutral', CircleCheck, 'Active'],
  revoked: ['muted', CircleX, 'Revoked'],
  replaced: ['muted', CircleX, 'Replaced'],
  expired: ['muted', CircleDashed, 'Expired'],
  exhausted: ['muted', CircleDashed, 'Used up'],
}

export function GrantStatusBadge({ grant }: { grant: Pick<Grant, 'status'> }) {
  const [tone, icon, label] = grantCopy[grant.status]
  return (
    <Badge tone={tone} icon={icon}>
      {label}
    </Badge>
  )
}

export function RoleBadge({ role }: { role: 'owner' | 'editor' | 'viewer' }) {
  return (
    <span className={`role role--${role}`}>
      {role === 'owner' ? 'Owner' : role === 'editor' ? 'Editor' : 'Viewer'}
    </span>
  )
}
