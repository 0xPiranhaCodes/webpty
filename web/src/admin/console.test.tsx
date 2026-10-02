import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, test } from 'vitest'

import * as fx from '../test/fixtures'
import { renderApp } from '../test/render'
import { adminServer } from '../test/scenarios'

describe('overview', () => {
  test('summarises real counts and derived setup findings', async () => {
    renderApp('/admin', adminServer())
    const ledger = await screen.findByText(/1 terminal running, 1 ended/)
    expect(ledger).toBeInTheDocument()
    expect(
      screen.getByText('2 participants across 1 terminal.'),
    ).toBeInTheDocument()
    expect(screen.getByText(/2 recordings stored/)).toBeInTheDocument()
    expect(screen.getByText(/Kept for 30 days/)).toBeInTheDocument()
    const findings = screen.getByRole('heading', {
      name: 'Setup and security',
    }).parentElement!
    expect(
      within(findings).getByText(/set WEBPTY_PUBLIC_ORIGIN/),
    ).toBeInTheDocument()
    expect(
      within(findings).getByText(/1 recording stopped early/),
    ).toBeInTheDocument()
  })

  test('offline is an actionable state with retry', async () => {
    const server = adminServer({
      'GET /api/v1/admin/terminals': () =>
        Promise.reject(new TypeError('Failed to fetch')),
    })
    renderApp('/admin', server)
    expect(
      await screen.findByRole('heading', {
        name: 'The server could not be reached.',
      }),
    ).toBeInTheDocument()
    server.set('GET /api/v1/admin/terminals', { body: { sessions: [] } })
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByText(/0 terminals running/)).toBeInTheDocument()
  })
})

describe('audit log', () => {
  test('shows event type, time, address, and details, and pages with the cursor', async () => {
    const server = adminServer({
      'GET /api/v1/admin/audit?limit=50&cursor=41': {
        body: {
          events: [
            {
              id: '40',
              type: 'admin.password.changed',
              occurredAt: '2026-10-01T04:00:30Z',
              remoteAddr: '127.0.0.1',
              details: {},
            },
          ],
          nextCursor: null,
        },
      },
    })
    renderApp('/admin/audit', server)
    const table = await screen.findByRole('table', { name: 'Audit events' })
    expect(within(table).getByText('access.grant.created')).toBeInTheDocument()
    expect(within(table).getByText('grantId=gr_editor1')).toBeInTheDocument()
    expect(within(table).getAllByText('127.0.0.1')).toHaveLength(2)

    await userEvent.click(
      screen.getByRole('button', { name: 'Load older events' }),
    )
    expect(
      await within(table).findByText('admin.password.changed'),
    ).toBeInTheDocument()
    await waitFor(() =>
      expect(
        screen.queryByRole('button', { name: 'Load older events' }),
      ).not.toBeInTheDocument(),
    )
    expect(
      screen.getByText('You have reached the oldest event.'),
    ).toBeInTheDocument()
  })

  test('refreshing starts again from the newest page', async () => {
    const server = adminServer({
      'GET /api/v1/admin/audit?limit=50&cursor=41': {
        body: {
          events: [
            {
              id: '40',
              type: 'admin.password.changed',
              occurredAt: '2026-10-01T04:00:30Z',
              remoteAddr: '127.0.0.1',
              details: {},
            },
          ],
          nextCursor: null,
        },
      },
    })
    renderApp('/admin/audit', server)
    const table = await screen.findByRole('table', { name: 'Audit events' })
    await userEvent.click(
      screen.getByRole('button', { name: 'Load older events' }),
    )
    expect(
      await within(table).findByText('admin.password.changed'),
    ).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() =>
      expect(
        server
          .calls('GET /api/v1/admin/audit')
          .filter((r) => !r.url.searchParams.has('cursor')),
      ).toHaveLength(2),
    )
    await waitFor(() =>
      expect(
        within(table).queryByText('admin.password.changed'),
      ).not.toBeInTheDocument(),
    )
    expect(
      screen.getByRole('button', { name: 'Load older events' }),
    ).toBeInTheDocument()
  })
})

describe('settings', () => {
  test('marks every value as restart-required and read-only', async () => {
    renderApp('/admin/settings', adminServer())
    expect(await screen.findByText('Listen address')).toBeInTheDocument()
    expect(screen.getAllByText('Restart required')).toHaveLength(
      fx.settings.settings.length,
    )
    expect(screen.getByText(/read-only here/)).toBeInTheDocument()
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
  })
})
