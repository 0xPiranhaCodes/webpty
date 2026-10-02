import type { ReactNode } from 'react'
import { Link } from 'react-router'

import type { Recording, RuntimeSettings, Terminal } from '../api/types'
import { plural } from '../lib/format'

export interface Finding {
  tone: 'warning' | 'info'
  text: ReactNode
}

export function setting(settings: RuntimeSettings, key: string) {
  return settings.settings.find((s) => s.key === key)?.value
}

/** Security and setup findings derived from real configuration and state. */
export function findings(
  terminals: Terminal[],
  recordings: Recording[],
  settings: RuntimeSettings,
): Finding[] {
  const out: Finding[] = []
  const origin = setting(settings, 'WEBPTY_PUBLIC_ORIGIN') ?? ''
  const running = terminals.filter((t) => t.state === 'running').length
  const max = Number(setting(settings, 'WEBPTY_MAX_SESSIONS'))
  const incomplete = recordings.filter((r) => r.status === 'incomplete').length

  if (
    origin.startsWith('http://') &&
    !/^http:\/\/(localhost|127\.|\[::1\])/.test(origin)
  ) {
    out.push({
      tone: 'warning',
      text: 'Shared links and sign-in travel over plain HTTP. Serve webpty behind HTTPS and set WEBPTY_PUBLIC_ORIGIN to the https:// address.',
    })
  } else if (!origin.startsWith('http')) {
    out.push({
      tone: 'info',
      text: 'Only this machine can reach the admin console. To share links with other devices, set WEBPTY_PUBLIC_ORIGIN and restart.',
    })
  }
  if (setting(settings, 'WEBPTY_RECORDING_ENABLED') === 'false') {
    out.push({
      tone: 'info',
      text: 'Recording is turned off. New terminals are not recorded.',
    })
  }
  if (incomplete > 0) {
    out.push({
      tone: 'warning',
      text: (
        <>
          {plural(incomplete, 'recording')} stopped early and{' '}
          {incomplete === 1 ? 'is' : 'are'} incomplete.{' '}
          <Link to="/admin/recordings">Review recordings</Link>
        </>
      ),
    })
  }
  if (Number.isFinite(max) && max > 0 && running >= max) {
    out.push({
      tone: 'warning',
      text: `All ${max} terminal slots are in use. New terminals will be refused until one ends.`,
    })
  }
  return out
}
