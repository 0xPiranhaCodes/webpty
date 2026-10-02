import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, test } from 'vitest'

import { createFakeServer } from '../test/fakeServer'
import * as fx from '../test/fixtures'
import { renderApp } from '../test/render'

const newPassword = 'correct horse battery staple'

function adminBackend() {
  return createFakeServer({
    'GET /api/v1/admin/session': { body: fx.adminSession },
    'GET /api/v1/admin/terminals': {
      body: { sessions: [fx.terminalRunning, fx.terminalExited] },
    },
    'GET /api/v1/admin/recordings': {
      body: { recordings: [fx.recordingComplete, fx.recordingIncomplete] },
    },
    'GET /api/v1/admin/settings': { body: fx.settings },
    'GET /api/v1/admin/audit': { body: fx.auditPage },
    'POST /api/v1/admin/logout': {},
  })
}

describe('admin sign-in', () => {
  test('an anonymous visitor sees the sign-in form, and a wrong password is explained without echoing it', async () => {
    const server = createFakeServer({
      'GET /api/v1/admin/session': {
        status: 401,
        body: { error: 'unauthenticated' },
      },
      'POST /api/v1/admin/login': {
        status: 401,
        body: { error: 'invalid credentials' },
      },
    })
    renderApp('/admin', server)

    const password = await screen.findByLabelText('Administrator password')
    await userEvent.type(password, 'guess-secret-1')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'That password is not correct.',
    )
    expect(document.body.textContent).not.toContain('guess-secret-1')
    expect(server.calls('POST /api/v1/admin/login')[0].body).toEqual({
      password: 'guess-secret-1',
    })
  })

  test('throttled sign-in tells the operator when to retry', async () => {
    const server = createFakeServer({
      'GET /api/v1/admin/session': {
        status: 401,
        body: { error: 'unauthenticated' },
      },
      'POST /api/v1/admin/login': {
        status: 429,
        body: { error: 'too many attempts' },
        headers: { 'Retry-After': '60' },
      },
    })
    renderApp('/admin', server)
    await userEvent.type(
      await screen.findByLabelText('Administrator password'),
      'x',
    )
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Too many attempts. Try again in 60 seconds.',
    )
  })

  test('an untrusted host explains the configuration fix', async () => {
    const server = createFakeServer({
      'GET /api/v1/admin/session': {
        status: 403,
        body: { error: 'untrusted host; configure WEBPTY_PUBLIC_ORIGIN' },
      },
    })
    renderApp('/admin', server)
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'WEBPTY_PUBLIC_ORIGIN',
    )
  })
})

describe('first-run bootstrap', () => {
  test('signing in with CHANGEME forces a new password before any admin page', async () => {
    let state = 'anonymous'
    const server = createFakeServer({
      'GET /api/v1/admin/session': () =>
        state === 'anonymous'
          ? { status: 401, body: { error: 'unauthenticated' } }
          : {
              body:
                state === 'bootstrap' ? fx.bootstrapSession : fx.adminSession,
            },
      'POST /api/v1/admin/login': () => {
        state = 'bootstrap'
        return { body: { passwordChangeRequired: true } }
      },
      'POST /api/v1/admin/password': () => {
        state = 'admin'
        return { status: 204 }
      },
      'GET /api/v1/admin/terminals': { body: { sessions: [] } },
      'GET /api/v1/admin/recordings': { body: { recordings: [] } },
      'GET /api/v1/admin/settings': { body: fx.settings },
    })
    renderApp('/admin/settings', server)

    await userEvent.type(
      await screen.findByLabelText('Administrator password'),
      'CHANGEME',
    )
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(
      await screen.findByRole('heading', {
        name: 'Set the administrator password',
      }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('navigation', { name: 'Admin' }),
    ).not.toBeInTheDocument()
    expect(server.calls('GET /api/v1/admin/settings')).toHaveLength(0)
    expect(screen.queryByLabelText('Current password')).not.toBeInTheDocument()

    const next = screen.getByLabelText('New password')
    const confirm = screen.getByLabelText('Confirm new password')
    const save = screen.getByRole('button', { name: 'Save password' })

    await userEvent.type(next, 'short')
    await userEvent.type(confirm, 'short')
    await userEvent.click(save)
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Use at least 12 characters.',
    )
    expect(server.calls('POST /api/v1/admin/password')).toHaveLength(0)

    await userEvent.clear(next)
    await userEvent.clear(confirm)
    await userEvent.type(next, newPassword)
    await userEvent.type(confirm, newPassword + 'x')
    await userEvent.click(save)
    expect(screen.getByRole('alert')).toHaveTextContent(
      'The passwords do not match.',
    )

    await userEvent.clear(confirm)
    await userEvent.type(confirm, newPassword)
    await userEvent.click(save)

    expect(
      await screen.findByRole('heading', { name: 'Settings' }),
    ).toBeInTheDocument()
    expect(server.calls('POST /api/v1/admin/password')[0].body).toEqual({
      currentPassword: 'CHANGEME',
      newPassword,
    })
  })

  test('after a reload during bootstrap the current password is asked for again', async () => {
    const server = createFakeServer({
      'GET /api/v1/admin/session': { body: fx.bootstrapSession },
    })
    renderApp('/admin', server)
    expect(await screen.findByLabelText('Current password')).toBeInTheDocument()
  })

  test('the default password is rejected as the new password', async () => {
    const server = createFakeServer({
      'GET /api/v1/admin/session': { body: fx.bootstrapSession },
    })
    renderApp('/admin', server)
    await userEvent.type(
      await screen.findByLabelText('Current password'),
      'CHANGEME',
    )
    await userEvent.type(screen.getByLabelText('New password'), 'CHANGEME')
    await userEvent.type(
      screen.getByLabelText('Confirm new password'),
      'CHANGEME',
    )
    await userEvent.click(screen.getByRole('button', { name: 'Save password' }))
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Choose a password other than the default.',
    )
  })
})

describe('signed-in shell', () => {
  test('navigation is labelled and sign-out sends the CSRF token and returns to sign-in', async () => {
    const server = adminBackend()
    renderApp('/admin', server)

    const nav = await screen.findByRole('navigation', { name: 'Admin' })
    for (const name of [
      'Overview',
      'Live sessions',
      'Recordings',
      'Access grants',
      'Audit log',
      'Settings',
    ]) {
      expect(
        within(nav).getByRole('link', { name: new RegExp(`^${name}`) }),
      ).toBeInTheDocument()
    }
    expect(
      within(nav).getByRole('link', { name: /^Overview/ }),
    ).toHaveAttribute('aria-current', 'page')

    server.set('GET /api/v1/admin/session', {
      status: 401,
      body: { error: 'unauthenticated' },
    })
    await userEvent.click(screen.getByRole('button', { name: 'Sign out' }))

    expect(
      await screen.findByLabelText('Administrator password'),
    ).toBeInTheDocument()
    expect(
      server.calls('POST /api/v1/admin/logout')[0].headers.get('X-CSRF-Token'),
    ).toBe('csrf-admin-token')
  })

  test('an expired session during use returns to sign-in', async () => {
    const server = adminBackend()
    server.set('GET /api/v1/admin/settings', {
      status: 401,
      body: { error: 'unauthenticated' },
    })
    renderApp('/admin/settings', server)
    expect(
      await screen.findByLabelText('Administrator password'),
    ).toBeInTheDocument()
    expect(
      await screen.findByText('Your session ended. Sign in again to continue.'),
    ).toBeInTheDocument()
  })

  test('a session that expires before a change returns to sign-in', async () => {
    const server = adminBackend()
    server.set('POST /api/v1/admin/recordings/retention/run', {
      status: 401,
      body: { error: 'unauthenticated' },
    })
    renderApp('/admin/recordings', server)
    await userEvent.click(
      await screen.findByRole('button', { name: 'Delete expired recordings' }),
    )
    await userEvent.click(
      within(screen.getByRole('alertdialog')).getByRole('button', {
        name: 'Delete expired',
      }),
    )
    expect(
      await screen.findByLabelText('Administrator password'),
    ).toBeInTheDocument()
    expect(
      screen.getByText('Your session ended. Sign in again to continue.'),
    ).toBeInTheDocument()
  })

  test('a wrong password while signing in is not treated as an ended session', async () => {
    const server = adminBackend()
    server.set('GET /api/v1/admin/session', {
      status: 401,
      body: { error: 'unauthenticated' },
    })
    server.set('POST /api/v1/admin/login', {
      status: 401,
      body: { error: 'invalid credentials' },
    })
    renderApp('/admin', server)
    await userEvent.type(
      await screen.findByLabelText('Administrator password'),
      'wrong',
    )
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() =>
      expect(server.calls('POST /api/v1/admin/login')).toHaveLength(1),
    )
    expect(
      screen.queryByText('Your session ended. Sign in again to continue.'),
    ).not.toBeInTheDocument()
  })

  test('unknown routes show a not-found page with a way back', async () => {
    renderApp('/nowhere', adminBackend())
    expect(
      await screen.findByRole('heading', { name: 'Page not found' }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('link', { name: 'Go to the admin overview' }),
    ).toHaveAttribute('href', '/admin')
  })

  test('the root redirects to the admin overview', async () => {
    renderApp('/', adminBackend())
    await waitFor(() =>
      expect(
        screen.getByRole('heading', { name: 'Overview' }),
      ).toBeInTheDocument(),
    )
  })
})
