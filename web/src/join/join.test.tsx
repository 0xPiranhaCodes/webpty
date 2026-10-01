import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, test } from 'vitest'

import { createFakeServer } from '../test/fakeServer'
import { FakeSocket, FakeXterm, resetTerminalFakes } from '../test/fakeTerminal'
import * as fx from '../test/fixtures'
import { renderApp } from '../test/render'
import { readyFrame, serverCloses, serverSends } from '../test/scenarios'
import { resetInviteStateForTests } from './invite'

beforeEach(() => {
  resetTerminalFakes()
  resetInviteStateForTests()
  window.history.replaceState(null, '', '/')
})

function guestServer(session = fx.guestSession) {
  return createFakeServer({
    'POST /api/v1/access/redeem': { body: session },
    'GET /api/v1/access/session': { body: session },
    'POST /api/v1/access/logout': {},
  })
}

describe('joining with an invitation', () => {
  test('the token leaves the address bar before redemption and is redeemed once', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    const server = guestServer()
    let hashAtRedeem: string | undefined
    server.set('POST /api/v1/access/redeem', () => {
      hashAtRedeem = window.location.hash
      return { body: fx.guestSession }
    })

    renderApp('/join', server)

    expect(await screen.findByText('You joined as a viewer.')).toBeInTheDocument()
    expect(hashAtRedeem).toBe('')
    expect(server.calls('POST /api/v1/access/redeem')).toHaveLength(1)
    expect(server.calls('POST /api/v1/access/redeem')[0].body).toEqual({ token: fx.inviteToken })
    expect(document.body.innerHTML).not.toContain(fx.inviteToken)
    expect(window.location.href).not.toContain(fx.inviteToken)
  })

  test('viewers see a persistent explanation and cannot type', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    renderApp('/join', guestServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('viewer'))

    expect(screen.getByText('You are viewing. Only the owner and the editor can type.')).toBeInTheDocument()
    expect(FakeXterm.last().inputEnabled).toBe(false)
    FakeXterm.last().type('rm -rf ~\r')
    expect(FakeSocket.last().sent).toEqual([])
    expect(FakeXterm.last().cols).toBe(120)
    expect(FakeXterm.last().rows).toBe(32)

    const hint = screen.getByText('Tab moves focus past the terminal.')
    expect(FakeXterm.last().describedBy).toBe(hint.id)
    expect(FakeXterm.last().keyHandler!(new KeyboardEvent('keydown', { key: 'Tab' }))).toBe(false)
    expect(FakeXterm.last().keyHandler!(new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true }))).toBe(false)
  })

  test('editors can type', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    const editor = { ...fx.guestSession, role: 'editor', permissions: { input: true, resize: true } }
    renderApp('/join', guestServer(editor))
    expect(await screen.findByText('You joined as an editor.')).toBeInTheDocument()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('editor'))
    FakeXterm.last().type('pwd\r')
    expect(FakeSocket.last().sent).toContain('{"type":"input","data":"pwd\\r"}')
  })

  test('an expired, revoked, or used link gives an actionable error without the token', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    const server = guestServer()
    server.set('POST /api/v1/access/redeem', { status: 401, body: { error: 'invalid or expired invitation', code: 'invalid_invitation' } })
    renderApp('/join', server)

    expect(await screen.findByRole('heading', { name: 'This link no longer works' })).toBeInTheDocument()
    expect(screen.getByText(/Ask the person who shared it for a new link/)).toBeInTheDocument()
    expect(document.body.innerHTML).not.toContain(fx.inviteToken)
  })

  test('a link for a terminal that ended says so', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    const server = guestServer()
    server.set('POST /api/v1/access/redeem', { status: 409, body: { error: 'terminal session is not running', code: 'invalid_state' } })
    renderApp('/join', server)
    expect(await screen.findByRole('heading', { name: 'This terminal has ended' })).toBeInTheDocument()
  })

  test('without a token an existing guest session resumes, otherwise the full link is asked for', async () => {
    const server = guestServer()
    renderApp('/join', server)
    expect(await screen.findByText('You joined as a viewer.')).toBeInTheDocument()
    expect(server.calls('POST /api/v1/access/redeem')).toHaveLength(0)
  })

  test('without a token or a session the visitor is told to open the complete link', async () => {
    const server = guestServer()
    server.set('GET /api/v1/access/session', { status: 401, body: { error: 'unauthenticated' } })
    renderApp('/join', server)
    expect(await screen.findByRole('heading', { name: 'Open the complete link' })).toBeInTheDocument()
  })

  test('live revocation ends the view with the reason', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    renderApp('/join', guestServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('viewer'), { type: 'presence_snapshot', version: 1, self: 'pt_me', participants: [{ id: 'pt_me', role: 'viewer' }] })
    serverSends({ type: 'permission_changed', version: 2, participant: { id: 'pt_me', role: 'viewer' }, permissions: { input: false, resize: false }, reason: 'revoked' })
    serverCloses(4003, 'access revoked')

    expect(screen.getByTestId('readout')).toHaveTextContent('Access revoked')
    expect(document.querySelector('.spine')).toHaveAttribute('data-state', 'lost')
  })

  test('viewers follow the owner’s screen size', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    renderApp('/join', guestServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('viewer'), { type: 'resize', rows: 40, cols: 100 })
    expect(FakeXterm.last().cols).toBe(100)
    expect(FakeXterm.last().rows).toBe(40)
  })

  test('a newer invitation opened in this browser ends this view with the reason', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    renderApp('/join', guestServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('viewer'))
    serverCloses(4003, 'access superseded')
    expect(screen.getByTestId('readout')).toHaveTextContent('Replaced by a newer invitation in this browser')
  })

  test('leaving signs the guest out with the guest CSRF token', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    const server = guestServer()
    renderApp('/join', server)
    await userEvent.click(await screen.findByRole('button', { name: 'Leave' }))
    expect(await screen.findByRole('heading', { name: 'You left the terminal' })).toBeInTheDocument()
    expect(server.calls('POST /api/v1/access/logout')[0].headers.get('X-CSRF-Token')).toBe('csrf-guest-token')
  })

  test('guests never see recording state', async () => {
    window.history.replaceState(null, '', `/join#token=${fx.inviteToken}`)
    renderApp('/join', guestServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('viewer'))
    expect(screen.getByTestId('readout')).toHaveTextContent('Live')
    expect(screen.queryByText(/record/i)).not.toBeInTheDocument()
  })
})
