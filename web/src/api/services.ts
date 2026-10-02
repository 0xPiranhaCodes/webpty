import { createContext, useContext } from 'react'

import { createApiClient } from './client'
import { type Api, createApi } from './endpoints'

export interface Services {
  api: Api
  /** Sets the CSRF token sent with mutations; undefined clears it. */
  setCsrf(token: string | undefined): void
  /** Subscribes to admin requests refused because the session ended; returns the unsubscribe. */
  onAdminSessionEnded(listener: () => void): () => void
}

export function createServices(
  options: { fetch?: typeof fetch } = {},
): Services {
  let csrf: string | undefined
  const listeners = new Set<() => void>()
  const client = createApiClient({
    fetch: options.fetch,
    csrf: () => csrf,
    onAdminSessionEnded: () => listeners.forEach((listener) => listener()),
  })
  return {
    api: createApi(client),
    setCsrf: (token) => {
      csrf = token
    },
    onAdminSessionEnded: (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}

export const ServicesContext = createContext<Services | null>(null)

export function useServices(): Services {
  const services = useContext(ServicesContext)
  if (!services) throw new Error('useServices outside ServicesProvider')
  return services
}

export const useApi = () => useServices().api
