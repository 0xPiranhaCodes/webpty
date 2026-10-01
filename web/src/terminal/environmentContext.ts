import { createContext, useContext, useSyncExternalStore } from 'react'

import type { WebSocketLike } from './connection'
import { createXterm, type XtermFactory } from './xterm'

export interface TerminalEnvironment {
  socketFactory?: (url: string, protocols: string[]) => WebSocketLike
  createXterm: XtermFactory
  /** Forces phone behaviour; otherwise the phone media query decides. */
  phone?: boolean
  origin?: string
}

/** Phones can watch, share, and end sessions but not type into them. */
export const phoneQuery = '(max-width: 640px), (pointer: coarse) and (max-width: 900px) and (max-height: 500px)'

export const EnvironmentContext = createContext<TerminalEnvironment>({ createXterm })

export const useTerminalEnvironment = () => useContext(EnvironmentContext)

function subscribe(query: string) {
  return (onChange: () => void) => {
    if (typeof window.matchMedia !== 'function') return () => {}
    const list = window.matchMedia(query)
    list.addEventListener('change', onChange)
    return () => list.removeEventListener('change', onChange)
  }
}

export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    subscribe(query),
    () => typeof window.matchMedia === 'function' && window.matchMedia(query).matches,
    () => false,
  )
}

export function useIsPhone(): boolean {
  const { phone } = useTerminalEnvironment()
  const matches = useMediaQuery(phoneQuery)
  return phone ?? matches
}
