import { defineConfig, devices } from '@playwright/test'

import { dataDir } from './e2e/servers'

// Each browser gets its own server and database: the specs walk one
// operator's history in order (first run, terminals, recordings), so two
// browsers sharing a server would see each other's state. Files run in name
// order and critical-paths must run first: it checks first-run setup.
const basePort = Number(process.env.WEBPTY_E2E_PORT ?? 8766)
const browsers = [
  { name: 'chromium', device: devices['Desktop Chrome'], port: basePort },
  { name: 'webkit', device: devices['Desktop Safari'], port: basePort + 1 },
]

export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: [['list']],
  use: { trace: 'retain-on-failure' },
  projects: browsers.map(({ name, device, port }) => ({
    name,
    use: { ...device, viewport: { width: 1440, height: 900 }, baseURL: `http://127.0.0.1:${port}` },
  })),
  webServer: browsers.map(({ port }, i) => ({
    // The first server builds the frontend once; serve.sh embeds whatever is in web/dist.
    command: i === 0 ? 'npm run build && sh e2e/serve.sh' : 'sh e2e/serve.sh',
    url: `http://127.0.0.1:${port}/healthz`,
    reuseExistingServer: false,
    timeout: 180_000,
    env: { WEBPTY_E2E_PORT: String(port), WEBPTY_E2E_DATA: dataDir(port) },
  })),
})
