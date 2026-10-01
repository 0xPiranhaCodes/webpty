import { CircleAlert, LoaderCircle, RefreshCw, WifiOff } from 'lucide-react'
import type { ReactNode } from 'react'

import type { ApiError } from '../api/client'
import { describeError } from './describeError'

export function Loading({ label }: { label: string }) {
  return (
    <div className="state state--loading" role="status" aria-live="polite">
      <LoaderCircle aria-hidden className="state__icon spin" size={18} />
      <span>{label}</span>
    </div>
  )
}

export function EmptyState({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="state state--empty">
      <h3>{title}</h3>
      {children && <p className="muted">{children}</p>}
      {action && <div className="state__action">{action}</div>}
    </div>
  )
}

export function ErrorState({ error, subject, onRetry, children }: { error: ApiError; subject?: string; onRetry?: () => void; children?: ReactNode }) {
  const copy = describeError(error, subject)
  const Icon = error.code === 'offline' ? WifiOff : CircleAlert
  return (
    <div className="state state--error" role="alert">
      <Icon aria-hidden className="state__icon" size={18} />
      <div className="state__text">
        <h3>{copy.title}</h3>
        <p className="muted">{copy.body}</p>
        {(onRetry || children) && (
          <div className="state__action">
            {onRetry && (
              <button type="button" className="btn" onClick={onRetry}>
                <RefreshCw aria-hidden size={14} /> Try again
              </button>
            )}
            {children}
          </div>
        )}
      </div>
    </div>
  )
}

export function InlineError({ children }: { children: ReactNode }) {
  return (
    <p className="inline-error" role="alert">
      <CircleAlert aria-hidden size={14} />
      <span>{children}</span>
    </p>
  )
}
