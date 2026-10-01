import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router'

import { AppRoutes } from '../App'
import { ServicesProvider } from '../api/context'
import { createServices } from '../api/services'
import { TerminalEnvironmentProvider } from '../terminal/environment'
import type { FakeServer } from './fakeServer'
import { FakeSocket, FakeXterm } from './fakeTerminal'

export function renderApp(path: string, server: FakeServer, options: { phone?: boolean } = {}) {
  const services = createServices({ fetch: server.fetch })
  return render(
    <ServicesProvider services={services}>
      <TerminalEnvironmentProvider
        value={{
          socketFactory: (url) => new FakeSocket(url),
          createXterm: (_container, options) => new FakeXterm(options),
          phone: options.phone ?? false,
          origin: 'http://127.0.0.1:8000',
        }}
      >
        <MemoryRouter initialEntries={[path]}>
          <AppRoutes />
        </MemoryRouter>
      </TerminalEnvironmentProvider>
    </ServicesProvider>,
  )
}
