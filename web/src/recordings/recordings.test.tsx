import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, test } from 'vitest'

import { FakeXterm, resetTerminalFakes } from '../test/fakeTerminal'
import * as fx from '../test/fixtures'
import { renderApp } from '../test/render'
import { adminServer } from '../test/scenarios'

beforeEach(resetTerminalFakes)

const eventsKey = 'GET /api/v1/admin/recordings/rc_91ab/events'

describe('recordings list', () => {
  test('shows status in words and flags incomplete recordings', async () => {
    renderApp('/admin/recordings', adminServer())
    const table = await screen.findByRole('table', { name: 'Recordings' })
    expect(within(table).getByText('Complete')).toBeInTheDocument()
    expect(within(table).getByText('Incomplete')).toBeInTheDocument()
    expect(within(table).getAllByRole('link', { name: /^Play recording/ })).toHaveLength(2)
  })

  test('running retention needs confirmation and reports the result', async () => {
    const server = adminServer({ 'POST /api/v1/admin/recordings/retention/run': { body: { deleted: 2 } } })
    renderApp('/admin/recordings', server)
    await userEvent.click(await screen.findByRole('button', { name: 'Delete expired recordings' }))
    const dialog = screen.getByRole('alertdialog', { name: 'Delete expired recordings now?' })
    await userEvent.click(within(dialog).getByRole('button', { name: 'Delete expired' }))
    expect(await screen.findByText('Deleted 2 expired recordings.')).toBeInTheDocument()
    expect(server.calls('POST /api/v1/admin/recordings/retention/run')[0].headers.get('X-CSRF-Token')).toBe('csrf-admin-token')
  })

  test('older recordings load from the cursor the server returns', async () => {
    const older = { ...fx.recordingComplete, id: 'rc_old1', startedAt: '2026-09-01T03:00:00Z' }
    const server = adminServer({
      'GET /api/v1/admin/recordings?limit=100': { body: { recordings: [fx.recordingComplete], nextCursor: 'rc_91ab' } },
      'GET /api/v1/admin/recordings?limit=100&before=rc_91ab': { body: { recordings: [older], nextCursor: null } },
    })
    renderApp('/admin/recordings', server)
    const table = await screen.findByRole('table', { name: 'Recordings' })
    expect(within(table).getAllByRole('link', { name: /^Play recording/ })).toHaveLength(1)

    await userEvent.click(screen.getByRole('button', { name: 'Load older recordings' }))
    await waitFor(() => expect(within(table).getAllByRole('link', { name: /^Play recording/ })).toHaveLength(2))
    expect(screen.queryByRole('button', { name: 'Load older recordings' })).not.toBeInTheDocument()
  })

  test('can be filtered to one terminal', async () => {
    const server = adminServer({ 'GET /api/v1/admin/recordings?terminalId=tm_7hd01a&limit=100': { body: { recordings: [fx.recordingComplete] } } })
    renderApp('/admin/recordings?terminal=tm_7hd01a', server)
    expect(await screen.findByText(/Showing recordings of terminal/)).toBeInTheDocument()
    expect(server.requests.some((r) => r.url.search === '?terminalId=tm_7hd01a&limit=100')).toBe(true)
  })
})

describe('recording playback', () => {
  function playbackServer(extra = {}) {
    return adminServer({
      'GET /api/v1/admin/recordings/rc_91ab': { body: fx.recordingComplete },
      [eventsKey]: { body: fx.recordingEventsPage },
      'DELETE /api/v1/admin/recordings/rc_91ab': { body: { ...fx.recordingComplete, status: 'deleted', deletedAt: '2026-10-01T05:00:00Z' } },
      ...extra,
    })
  }

  test('is clearly marked as playback, not live', async () => {
    renderApp('/admin/recordings/rc_91ab', playbackServer())
    expect(await screen.findByRole('region', { name: 'Recording playback' })).toBeInTheDocument()
    await waitFor(() => expect(screen.getByTestId('readout')).toHaveTextContent('Playback'))
    expect(screen.getByTestId('readout')).not.toHaveTextContent('Live')
    expect(document.querySelector('.spine')).toHaveAttribute('data-state', 'playback')
    expect(FakeXterm.last().inputEnabled).toBe(false)
    await waitFor(() => expect(FakeXterm.last().written).toBe('$ '))
  })

  test('the recorded terminal is skipped by Tab so focus goes from the header to the transport', async () => {
    renderApp('/admin/recordings/rc_91ab', playbackServer())
    await screen.findByRole('button', { name: 'Play' })
    const xterm = FakeXterm.last()
    expect(xterm.tabStop).toBe(false)
    expect(xterm.keyHandler!(new KeyboardEvent('keydown', { key: 'Tab' }))).toBe(false)
    const region = screen.getByRole('region', { name: 'Recording playback' })
    expect(region.querySelector('[data-render]')).toHaveAttribute('data-render', 'idle')
  })

  test('has keyboard-complete transport controls', async () => {
    renderApp('/admin/recordings/rc_91ab', playbackServer())
    const play = await screen.findByRole('button', { name: 'Play' })
    const position = screen.getByRole('slider', { name: 'Position' })
    expect(position).toHaveAttribute('aria-valuetext', '0:00 of 26:12')

    const speed = screen.getByRole('radiogroup', { name: 'Playback speed' })
    const options = within(speed).getAllByRole('radio')
    expect(options.map((o) => o.getAttribute('aria-label') ?? o.parentElement?.textContent)).toEqual(['0.5×', '1×', '1.5×', '2×'])
    expect(within(speed).getByLabelText('1×')).toBeChecked()
    await userEvent.click(within(speed).getByLabelText('2×'))
    expect(within(speed).getByLabelText('2×')).toBeChecked()

    await userEvent.click(play)
    expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Pause' }))

    act(() => {
      position.focus()
    })
    await userEvent.keyboard('{End}')
    await waitFor(() => expect(FakeXterm.last().written).toContain('README.md'))
    expect(screen.getByText('26:12 / 26:12')).toBeInTheDocument()
  })

  test('export and delete are available, and delete needs confirmation', async () => {
    const server = playbackServer()
    renderApp('/admin/recordings/rc_91ab', server)
    expect(await screen.findByRole('link', { name: 'Export as asciicast' })).toHaveAttribute('href', '/api/v1/admin/recordings/rc_91ab/export')

    await userEvent.click(screen.getByRole('button', { name: 'Delete recording' }))
    const dialog = screen.getByRole('alertdialog', { name: 'Delete this recording?' })
    await userEvent.click(within(dialog).getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(server.calls('DELETE /api/v1/admin/recordings/rc_91ab')).toHaveLength(1))
    expect(await screen.findByRole('table', { name: 'Recordings' })).toBeInTheDocument()
    expect(screen.getByText('Recording deleted.')).toBeInTheDocument()
  })

  test('an incomplete recording warns that playback ends early', async () => {
    const server = adminServer({
      'GET /api/v1/admin/recordings/rc_22cd': { body: fx.recordingIncomplete },
      'GET /api/v1/admin/recordings/rc_22cd/events': { body: { ...fx.recordingEventsPage, recording: fx.recordingIncomplete } },
    })
    renderApp('/admin/recordings/rc_22cd', server)
    expect(await screen.findByText(/This recording is incomplete/)).toHaveTextContent('output arrived faster than it could be stored')
  })

  test.each([
    [410, 'recording_deleted', 'recording was deleted', 'This recording was deleted.'],
    [422, 'recording_corrupt', 'recording failed validation and was marked incomplete', 'This recording failed validation.'],
    [409, 'recording_active', 'recording is still in progress', 'This recording is still in progress.'],
    [404, 'not_found', 'recording not found', 'This recording was not found.'],
  ])('a %i %s response is explained', async (status, code, error, title) => {
    const server = adminServer({
      'GET /api/v1/admin/recordings/rc_91ab': { body: fx.recordingComplete },
      [eventsKey]: { status, body: { error, code } },
    })
    renderApp('/admin/recordings/rc_91ab', server)
    expect(await screen.findByRole('heading', { name: title })).toBeInTheDocument()
  })
})
