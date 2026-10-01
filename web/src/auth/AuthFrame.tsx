import type { ReactNode } from 'react'

import { SignalSpine } from '../components/SignalSpine'

/** The frame for sign-in and first-run: the product's own spine, not a marketing card. */
export function AuthFrame({ children }: { children: ReactNode }) {
  return (
    <div className="auth-frame">
      <SignalSpine state="idle" />
      <main className="auth-frame__main" id="main">
        <p className="wordmark" aria-hidden="true">
          webpty
        </p>
        {children}
      </main>
    </div>
  )
}
