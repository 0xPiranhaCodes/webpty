import '@fontsource/ibm-plex-sans/latin-400.css'
import '@fontsource/ibm-plex-sans/latin-500.css'
import '@fontsource/ibm-plex-sans/latin-600.css'
import '@fontsource/ibm-plex-mono/latin-400.css'
import '@fontsource/ibm-plex-mono/latin-500.css'
import './styles/tokens.css'
import './styles/base.css'
import './styles/app.css'

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { ServicesProvider } from './api/context'
import { createServices } from './api/services'
import App from './App'
import { stashInviteFromLocation } from './join/invite'

// Runs before React so an invitation token never outlives the first tick in
// the address bar, history, or anything that reads location later.
stashInviteFromLocation()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ServicesProvider services={createServices()}>
      <App />
    </ServicesProvider>
  </StrictMode>,
)
