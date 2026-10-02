import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, test } from 'vitest'

import { resetTerminalFakes } from '../test/fakeTerminal'
import * as fx from '../test/fixtures'
import { renderApp } from '../test/render'
import { adminServer } from '../test/scenarios'

beforeEach(resetTerminalFakes)

const viewerCreated = {
  grant: { ...fx.viewerGrant, id: 'gr_new', label: 'Standup' },
  token: fx.inviteToken,
  inviteUrl: `http://127.0.0.1:8000/join#token=${fx.inviteToken}`,
  replaced: [],
}

describe('access grants', () => {
  test('the page picks a running terminal and lists its links without secrets', async () => {
    renderApp('/admin/access', adminServer())
    const select = await screen.findByLabelText('Terminal')
    expect(select).toHaveValue('tm_3kq9x2')
    const list = await screen.findByRole('list', {
      name: 'Links for this terminal',
    })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    expect(within(list).getByText('Pairing with Ana')).toBeInTheDocument()
    expect(within(list).getByText('Used 3 of 100')).toBeInTheDocument()
    expect(document.body.innerHTML).not.toContain(fx.inviteToken)
  })

  test('a new link is shown once with a copy control and then forgotten', async () => {
    const server = adminServer({
      'POST /api/v1/admin/terminals/tm_3kq9x2/grants': {
        status: 201,
        body: viewerCreated,
      },
    })
    renderApp('/admin/access', server)
    await screen.findByRole('list', { name: 'Links for this terminal' })

    await userEvent.click(screen.getByLabelText('Viewer'))
    await userEvent.type(screen.getByLabelText('Label'), 'Standup')
    await userEvent.selectOptions(
      screen.getByLabelText('Expires after'),
      '28800',
    )
    await userEvent.click(screen.getByRole('button', { name: 'Create link' }))

    await waitFor(() =>
      expect(
        server.calls('POST /api/v1/admin/terminals/tm_3kq9x2/grants'),
      ).toHaveLength(1),
    )
    expect(
      server.calls('POST /api/v1/admin/terminals/tm_3kq9x2/grants')[0].body,
    ).toEqual({
      role: 'viewer',
      label: 'Standup',
      ttlSeconds: 28800,
      singleUse: false,
    })
    const field = await screen.findByLabelText('Viewer link')
    expect(field).toHaveValue(viewerCreated.inviteUrl)
    expect(
      screen.getByRole('button', { name: 'Copy link' }),
    ).toBeInTheDocument()
    expect(
      screen.getByText('Copy it now. It is shown only once.'),
    ).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByLabelText('Viewer link')).not.toBeInTheDocument()
    expect(document.body.innerHTML).not.toContain(fx.inviteToken)
  })

  test('replacing the active editor needs confirmation', async () => {
    const server = adminServer({
      'POST /api/v1/admin/terminals/tm_3kq9x2/grants': {
        status: 201,
        body: {
          ...fx.createdGrant,
          grant: { ...fx.editorGrant, id: 'gr_editor2' },
        },
      },
    })
    renderApp('/admin/access', server)
    await screen.findByRole('list', { name: 'Links for this terminal' })

    await userEvent.click(screen.getByLabelText('Editor'))
    expect(screen.getByLabelText('Single use')).toBeChecked()
    await userEvent.click(screen.getByRole('button', { name: 'Create link' }))

    const dialog = screen.getByRole('alertdialog', {
      name: 'Replace the current editor?',
    })
    expect(dialog).toHaveTextContent('Pairing with Ana')
    expect(
      server.calls('POST /api/v1/admin/terminals/tm_3kq9x2/grants'),
    ).toHaveLength(0)
    await userEvent.click(
      within(dialog).getByRole('button', { name: 'Replace editor' }),
    )

    await waitFor(() =>
      expect(
        server.calls('POST /api/v1/admin/terminals/tm_3kq9x2/grants'),
      ).toHaveLength(1),
    )
    expect(
      server.calls('POST /api/v1/admin/terminals/tm_3kq9x2/grants')[0].body,
    ).toMatchObject({ role: 'editor', singleUse: true })
    expect(await screen.findByLabelText('Editor link')).toBeInTheDocument()
  })

  test('revoking a link needs confirmation and disconnects its users', async () => {
    const server = adminServer({
      'DELETE /api/v1/admin/terminals/tm_3kq9x2/grants/gr_viewer1': {
        body: {
          ...fx.viewerGrant,
          status: 'revoked',
          revokedAt: '2026-10-01T04:30:00Z',
        },
      },
    })
    renderApp('/admin/access', server)
    const list = await screen.findByRole('list', {
      name: 'Links for this terminal',
    })
    const viewerItem = within(list).getAllByRole('listitem')[1]
    await userEvent.click(
      within(viewerItem).getByRole('button', { name: /^Revoke/ }),
    )

    const dialog = screen.getByRole('alertdialog', {
      name: 'Revoke this link?',
    })
    expect(dialog).toHaveTextContent('disconnected immediately')
    await userEvent.click(
      within(dialog).getByRole('button', { name: 'Revoke' }),
    )
    await waitFor(() =>
      expect(
        server.calls(
          'DELETE /api/v1/admin/terminals/tm_3kq9x2/grants/gr_viewer1',
        ),
      ).toHaveLength(1),
    )
  })

  test('ended links are kept out of the way under their own disclosure', async () => {
    const revoked = {
      ...fx.viewerGrant,
      id: 'gr_old',
      label: 'Old viewer',
      status: 'revoked',
      revokedAt: '2026-10-01T04:20:00Z',
    }
    renderApp(
      '/admin/access',
      adminServer({
        'GET /api/v1/admin/terminals/tm_3kq9x2/grants': {
          body: { grants: [fx.editorGrant, revoked] },
        },
      }),
    )
    const active = await screen.findByRole('list', {
      name: 'Links for this terminal',
    })
    expect(within(active).getAllByRole('listitem')).toHaveLength(1)
    expect(screen.getByText('Ended links (1)')).toBeInTheDocument()
    const ended = screen.getByRole('list', {
      name: 'Ended links',
      hidden: true,
    })
    expect(within(ended).getByText('Revoked')).toBeInTheDocument()
    expect(
      within(ended).queryByRole('button', { name: /^Revoke/ }),
    ).not.toBeInTheDocument()
  })

  test('without running terminals the page explains why no link can be made', async () => {
    renderApp(
      '/admin/access',
      adminServer({
        'GET /api/v1/admin/terminals': {
          body: { sessions: [fx.terminalExited] },
        },
      }),
    )
    expect(await screen.findByText('No running terminals')).toBeInTheDocument()
  })
})
