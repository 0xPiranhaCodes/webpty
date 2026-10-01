import type { ReactNode } from 'react'

import { EnvironmentContext, type TerminalEnvironment } from './environmentContext'

export function TerminalEnvironmentProvider({ value, children }: { value: TerminalEnvironment; children: ReactNode }) {
  return <EnvironmentContext.Provider value={value}>{children}</EnvironmentContext.Provider>
}
