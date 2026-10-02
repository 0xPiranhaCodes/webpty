import { RotateCcw } from 'lucide-react'

import { useApi } from '../api/services'
import type { Setting } from '../api/types'
import { ErrorState, Loading } from '../components/StateViews'
import { useAsync } from '../lib/useAsync'

export function SettingsPage() {
  const api = useApi()
  const data = useAsync('settings', (signal) => api.settings(signal))
  const groups = new Map<string, Setting[]>()
  for (const s of data.data?.settings ?? [])
    groups.set(s.group, [...(groups.get(s.group) ?? []), s])

  return (
    <div className="page">
      <header className="page__header">
        <h1>Settings</h1>
        <p className="muted">
          These values are read from the environment when webpty starts and are
          read-only here. To change one, set the variable and restart webpty.
          Passwords, tokens, and command arguments are never shown.
        </p>
      </header>
      {data.status === 'loading' && !data.data && (
        <Loading label="Loading settings" />
      )}
      {data.status === 'error' && (
        <ErrorState error={data.error} onRetry={data.reload} />
      )}
      {[...groups].map(([group, settings]) => (
        <section
          key={group}
          className="section"
          aria-labelledby={`settings-${group}`}
        >
          <h2 id={`settings-${group}`}>{group}</h2>
          <dl className="settings">
            {settings.map((s) => (
              <div key={s.key} className="settings__row">
                <dt>
                  {s.label}
                  <code className="settings__key">{s.key}</code>
                </dt>
                <dd>
                  <span className="mono">{s.value}</span>
                  {s.restartRequired && (
                    <span className="settings__restart">
                      <RotateCcw aria-hidden size={12} /> Restart required
                    </span>
                  )}
                </dd>
              </div>
            ))}
          </dl>
        </section>
      ))}
    </div>
  )
}
