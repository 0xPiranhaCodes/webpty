import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, test } from 'vitest'

import { FakeSocket, FakeXterm, resetTerminalFakes } from '../test/fakeTerminal'
import * as fx from '../test/fixtures'
import { renderApp } from '../test/render'
import { adminServer, b64, readyFrame, serverCloses, serverSends } from '../test/scenarios'

beforeEach(resetTerminalFakes)

describe('live sessions', () => {
  test('lists terminals with state and people, and termination needs confirmation', async () => {
    const server = adminServer({
      'DELETE /api/v1/admin/terminals/tm_3kq9x2': { body: { ...fx.terminalRunning, state: 'terminated', participants: 0 } },
    })
    renderApp('/admin/sessions', server)

    const row = (await screen.findByText('/bin/zsh -l')).closest('tr')!
    expect(within(row).getByText('Running')).toBeInTheDocument()
    expect(within(row).getByText('2')).toBeInTheDocument()
    expect(screen.getByText('Exited 0')).toBeInTheDocument()

    await userEvent.click(within(row).getByRole('button', { name: 'Terminate /bin/zsh -l' }))
    const dialog = screen.getByRole('alertdialog', { name: 'Terminate this terminal?' })
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus()
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(server.calls('DELETE /api/v1/admin/terminals/tm_3kq9x2')).toHaveLength(0)

    await userEvent.click(within(row).getByRole('button', { name: 'Terminate /bin/zsh -l' }))
    await userEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Terminate' }))
    await waitFor(() => expect(server.calls('DELETE /api/v1/admin/terminals/tm_3kq9x2')).toHaveLength(1))
    expect(server.calls('DELETE /api/v1/admin/terminals/tm_3kq9x2')[0].headers.get('X-CSRF-Token')).toBe('csrf-admin-token')
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument())
  })

  test('creating a terminal sends the exact command and arguments and opens it', async () => {
    const server = adminServer({ 'POST /api/v1/admin/terminals': { status: 201, body: fx.terminalRunning } })
    renderApp('/admin/sessions', server)

    await userEvent.click(await screen.findByRole('button', { name: 'New terminal' }))
    await userEvent.type(screen.getByLabelText('Command'), '/bin/zsh')
    await userEvent.type(screen.getByLabelText('Arguments'), '-l{Enter}--no-rcs')
    await userEvent.type(screen.getByLabelText('Rows'), '40')
    await userEvent.type(screen.getByLabelText('Columns'), '132')
    await userEvent.click(screen.getByLabelText('Record this terminal'))
    await userEvent.click(screen.getByRole('button', { name: 'Start terminal' }))

    await waitFor(() => expect(server.calls('POST /api/v1/admin/terminals')).toHaveLength(1))
    expect(server.calls('POST /api/v1/admin/terminals')[0].body).toEqual({
      command: '/bin/zsh',
      args: ['-l', '--no-rcs'],
      rows: 40,
      cols: 132,
      record: false,
    })
    expect(await screen.findByRole('region', { name: 'Terminal' })).toBeInTheDocument()
  })

  test('the session limit is explained', async () => {
    const server = adminServer({
      'POST /api/v1/admin/terminals': { status: 429, body: { error: 'active terminal session limit reached', code: 'session_limit' } },
    })
    renderApp('/admin/sessions', server)
    await userEvent.click(await screen.findByRole('button', { name: 'New terminal' }))
    await userEvent.click(screen.getByRole('button', { name: 'Start terminal' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('WEBPTY_MAX_SESSIONS')
  })

  test('an empty list invites starting a terminal', async () => {
    renderApp('/admin/sessions', adminServer({ 'GET /api/v1/admin/terminals': { body: { sessions: [] } } }))
    expect(await screen.findByText('No terminals yet')).toBeInTheDocument()
  })
})

describe('owner workspace', () => {
  test('connects, renders output, and forwards typing and the fitted size', async () => {
    renderApp('/admin/sessions/tm_3kq9x2', adminServer())

    expect(await screen.findByRole('region', { name: 'Terminal' })).toBeInTheDocument()
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    expect(FakeSocket.last().url).toBe('ws://127.0.0.1:8000/api/v1/terminals/tm_3kq9x2/ws')
    expect(screen.getByTestId('readout')).toHaveTextContent('Connecting')

    serverSends(readyFrame('owner'), { type: 'output', seq: 1, data: b64('$ whoami\r\n') })
    expect(screen.getByTestId('readout')).toHaveTextContent('Live')
    expect(document.querySelector('.spine')).toHaveAttribute('data-state', 'live')
    await waitFor(() => expect(FakeXterm.last().written).toBe('$ whoami\r\n'))
    expect(FakeSocket.last().sent).toContain('{"type":"resize","rows":32,"cols":120}')

    FakeXterm.last().type('ls\r')
    expect(FakeSocket.last().sent).toContain('{"type":"input","data":"ls\\r"}')
  })

  test('a replay gap redraws from the new stream only, even while old output is still being parsed', async () => {
    FakeXterm.manual = true
    renderApp('/admin/sessions/tm_3kq9x2', adminServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(
      readyFrame('owner'),
      { type: 'output', seq: 1, data: b64('old-1 ') },
      { type: 'output', seq: 2, data: b64('old-2 ') },
    )
    const xterm = FakeXterm.last()
    expect(xterm.queued).toBe(1)

    serverSends({ type: 'error', code: 'replay_gap', message: 'requested output is no longer buffered', firstSeq: 9, lastSeq: 20 })
    serverCloses(4001, 'replay gap')
    await waitFor(() => expect(FakeSocket.all).toHaveLength(2))
    serverSends(readyFrame('owner', { seq: 20 }), { type: 'output', seq: 21, data: b64('fresh') })

    act(() => xterm.drainAll())
    expect(xterm.written).toBe('fresh')
    expect(screen.getByText(/Some output was missed while reconnecting/)).toBeInTheDocument()
  })

  test('an editable terminal explains how to move focus out, and keeps Tab for the shell', async () => {
    renderApp('/admin/sessions/tm_3kq9x2', adminServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('owner'))
    const xterm = FakeXterm.last()
    const hint = screen.getByText('Press Escape, then Tab, to move focus out of the terminal.')
    expect(xterm.describedBy).toBe(hint.id)
    expect(xterm.tabStop).toBe(true)
    expect(xterm.keyHandler!(new KeyboardEvent('keydown', { key: 'Tab' }))).toBe(true)
    xterm.keyHandler!(new KeyboardEvent('keydown', { key: 'Escape' }))
    expect(xterm.keyHandler!(new KeyboardEvent('keydown', { key: 'Tab' }))).toBe(false)
  })

  test('shows participants with roles and marks the current user', async () => {
    renderApp('/admin/sessions/tm_3kq9x2', adminServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('owner'), {
      type: 'presence_snapshot',
      version: 3,
      self: 'pt_owner1',
      participants: [
        { id: 'pt_owner1', role: 'owner' },
        { id: 'pt_edit42', role: 'editor' },
        { id: 'pt_view77', role: 'viewer' },
      ],
    })
    const rail = screen.getByRole('list', { name: 'People in this terminal' })
    const items = within(rail).getAllByRole('listitem')
    expect(items).toHaveLength(3)
    expect(items[0]).toHaveTextContent('You')
    expect(items[0]).toHaveTextContent('Owner')
    expect(items[1]).toHaveTextContent('Editor')
    expect(items[2]).toHaveTextContent('Viewer')

    serverSends({ type: 'participant_joined', version: 4, participant: { id: 'pt_owner2', role: 'owner' } })
    expect(within(rail).getByText('You, in another tab or device')).toBeInTheDocument()
  })

  test('reconnecting and recording warnings are announced in words', async () => {
    renderApp('/admin/sessions/tm_3kq9x2', adminServer({
      'GET /api/v1/admin/recordings?terminalId=tm_3kq9x2&limit=5': {
        body: { recordings: [{ ...fx.recordingComplete, terminalId: 'tm_3kq9x2', status: 'recording', endedAt: null }] },
      },
    }))
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('owner'))
    await waitFor(() => expect(screen.getByTestId('readout')).toHaveTextContent('Recording'))
    expect(document.querySelector('.spine')).toHaveAttribute('data-state', 'live-recording')

    serverSends({
      type: 'recording_status',
      recordingId: 'rc_91ab',
      status: 'incomplete',
      code: 'queue_overflow',
      message: 'recording stopped: output arrived faster than it could be stored',
    })
    expect(screen.getByText('recording stopped: output arrived faster than it could be stored')).toBeInTheDocument()
    expect(screen.getByTestId('readout')).not.toHaveTextContent('Recording')

    serverCloses(1006)
    expect(screen.getByTestId('readout')).toHaveTextContent('Reconnecting')
    expect(document.querySelector('.spine')).toHaveAttribute('data-state', 'reconnecting')
  })

  test('process exit is shown and typing stops', async () => {
    renderApp('/admin/sessions/tm_3kq9x2', adminServer())
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('owner'), { type: 'exit', state: 'exited', exitCode: 130 })
    expect(FakeXterm.last().inputEnabled).toBe(false)
    FakeXterm.last().type('x')
    expect(FakeSocket.last().sent.some((s) => s.includes('"input"'))).toBe(false)
    expect(screen.getByRole('region', { name: 'Terminal' }).querySelector('.workspace__screen')).toHaveAttribute('data-input', 'off')
    serverCloses(1000, 'session ended')
    expect(screen.getByTestId('readout')).toHaveTextContent('Ended with exit code 130')
    expect(FakeXterm.last().inputEnabled).toBe(false)
  })

  test('on a phone the owner can watch but typing is off with an explanation', async () => {
    renderApp('/admin/sessions/tm_3kq9x2', adminServer(), { phone: true })
    await screen.findByRole('region', { name: 'Terminal' })
    await waitFor(() => expect(FakeSocket.all).toHaveLength(1))
    serverSends(readyFrame('owner'))
    expect(screen.getByText(/Typing is off on phone-sized screens/)).toBeInTheDocument()
    expect(FakeXterm.last().inputEnabled).toBe(false)
    FakeXterm.last().type('x')
    expect(FakeSocket.last().sent.some((s) => s.includes('"input"'))).toBe(false)
    expect(FakeSocket.last().sent.some((s) => s.includes('"resize"'))).toBe(false)
    expect(screen.getByRole('button', { name: 'Terminate' })).toBeInTheDocument()
  })

  test('an ended terminal offers its recordings instead of connecting', async () => {
    renderApp('/admin/sessions/tm_7hd01a', adminServer({ 'GET /api/v1/admin/terminals/tm_7hd01a': { body: fx.terminalExited } }))
    expect(await screen.findByText('This terminal has ended.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'View its recordings' })).toHaveAttribute('href', '/admin/recordings?terminal=tm_7hd01a')
    expect(FakeSocket.all).toHaveLength(0)
  })
})
