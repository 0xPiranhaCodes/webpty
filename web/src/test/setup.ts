import '@testing-library/jest-dom/vitest'

import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// Node 25+ defines its own localStorage/sessionStorage globals, which shadow
// jsdom's and are undefined without --localstorage-file.
const dom = (globalThis as { jsdom?: { window: Window } }).jsdom?.window
for (const name of ['localStorage', 'sessionStorage'] as const) {
  if (!globalThis[name] && dom)
    Object.defineProperty(globalThis, name, {
      configurable: true,
      value: dom[name],
    })
}

afterEach(() => {
  cleanup()
})
