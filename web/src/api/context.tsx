import type { ReactNode } from 'react'

import { type Services, ServicesContext } from './services'

export function ServicesProvider({ services, children }: { services: Services; children: ReactNode }) {
  return <ServicesContext.Provider value={services}>{children}</ServicesContext.Provider>
}
