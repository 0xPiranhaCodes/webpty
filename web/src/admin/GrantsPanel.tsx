import { Link2 } from 'lucide-react'
import { type FormEvent, useId, useState } from 'react'

import { useApi } from '../api/services'
import type { CreatedGrant, Grant, GrantRole } from '../api/types'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { CopyField } from '../components/CopyField'
import { describeError } from '../components/describeError'
import { ErrorState, InlineError, Loading } from '../components/StateViews'
import { GrantStatusBadge, RoleBadge } from '../components/StatusBadge'
import { formatDateTime } from '../lib/format'
import { toApiError, useAsync } from '../lib/useAsync'

const lifetimes = [
  { seconds: 3600, label: '1 hour' },
  { seconds: 28800, label: '8 hours' },
  { seconds: 86400, label: '1 day' },
  { seconds: 604800, label: '7 days' },
]

const roleName = (role: GrantRole) => (role === 'editor' ? 'Editor' : 'Viewer')
const grantName = (grant: Grant) =>
  grant.label || `Unlabeled ${grant.role} link`

function GrantItem({
  grant,
  onRevoke,
}: {
  grant: Grant
  onRevoke: (grant: Grant) => void
}) {
  return (
    <li className="grant" data-status={grant.status}>
      <div className="grant__head">
        <RoleBadge role={grant.role} />
        <span className="grant__name">{grantName(grant)}</span>
        {grant.status !== 'active' && <GrantStatusBadge grant={grant} />}
      </div>
      <p className="grant__meta muted">
        <span>{`Used ${grant.redemptionCount} of ${grant.maxRedemptions}`}</span>
        <span>
          {grant.status === 'active'
            ? `Expires ${formatDateTime(grant.expiresAt)}`
            : `Ended ${formatDateTime(grant.revokedAt ?? grant.updatedAt)}`}
        </span>
      </p>
      {grant.status === 'active' && (
        <button
          type="button"
          className="btn btn--quiet btn--destructive-text"
          onClick={() => onRevoke(grant)}
          aria-label={`Revoke ${grantName(grant)}`}
        >
          Revoke
        </button>
      )}
    </li>
  )
}

function GrantList({
  grants,
  onRevoke,
}: {
  grants: Grant[]
  onRevoke: (grant: Grant) => void
}) {
  const active = grants.filter((g) => g.status === 'active')
  const ended = grants.filter((g) => g.status !== 'active')
  return (
    <>
      {active.length === 0 ? (
        <p className="muted">
          No active links. People you share a link with appear under People
          while connected.
        </p>
      ) : (
        <ul className="grant-list" aria-label="Links for this terminal">
          {active.map((grant) => (
            <GrantItem key={grant.id} grant={grant} onRevoke={onRevoke} />
          ))}
        </ul>
      )}
      {ended.length > 0 && (
        <details className="ended-grants">
          <summary>{`Ended links (${ended.length})`}</summary>
          <ul className="grant-list" aria-label="Ended links">
            {ended.map((grant) => (
              <GrantItem key={grant.id} grant={grant} onRevoke={onRevoke} />
            ))}
          </ul>
        </details>
      )}
    </>
  )
}

/**
 * Creates, lists, and revokes share links for one terminal. The raw link
 * exists only in the create response and is dropped when dismissed.
 */
export function GrantsPanel({
  terminalId,
  compact = false,
}: {
  terminalId: string
  compact?: boolean
}) {
  const api = useApi()
  const ids = { role: useId(), label: useId(), ttl: useId(), single: useId() }
  const grants = useAsync(`grants:${terminalId}`, (signal) =>
    api.listGrants(terminalId, signal),
  )
  const [role, setRole] = useState<GrantRole>('viewer')
  const [label, setLabel] = useState('')
  const [ttl, setTtl] = useState(86400)
  const [singleUse, setSingleUse] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [created, setCreated] = useState<CreatedGrant | null>(null)
  const [confirmReplace, setConfirmReplace] = useState<Grant | null>(null)
  const [revoking, setRevoking] = useState<Grant | null>(null)

  const activeEditor =
    grants.data?.find((g) => g.role === 'editor' && g.status === 'active') ??
    null

  const chooseRole = (next: GrantRole) => {
    setRole(next)
    setSingleUse(next === 'editor')
  }

  const create = async () => {
    setPending(true)
    setError(null)
    try {
      const result = await api.createGrant(terminalId, {
        role,
        ...(label.trim() ? { label: label.trim() } : {}),
        ttlSeconds: ttl,
        singleUse,
      })
      setCreated(result)
      setLabel('')
      grants.reload()
    } catch (e) {
      const copy = describeError(toApiError(e), 'terminal')
      setError(`${copy.title} ${copy.body}`)
    } finally {
      setPending(false)
      setConfirmReplace(null)
    }
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (role === 'editor' && activeEditor) setConfirmReplace(activeEditor)
    else void create()
  }

  return (
    <section
      className={compact ? 'rail-section grants grants--compact' : 'grants'}
      aria-labelledby={`${ids.role}-h`}
    >
      <h2 id={`${ids.role}-h`}>Share links</h2>

      {created ? (
        <div className="invite">
          <CopyField
            label={`${roleName(created.grant.role)} link`}
            value={created.inviteUrl}
          />
          <p className="invite__warning">Copy it now. It is shown only once.</p>
          {created.replaced.length > 0 && (
            <p className="muted">
              The previous editor link was replaced and its user disconnected.
            </p>
          )}
          <button
            type="button"
            className="btn"
            onClick={() => setCreated(null)}
          >
            Done
          </button>
        </div>
      ) : (
        <form className="form grant-form" onSubmit={submit}>
          <fieldset className="segmented">
            <legend>Role</legend>
            {(['viewer', 'editor'] as const).map((r) => (
              <label key={r} className="segmented__option">
                <input
                  type="radio"
                  name={`${ids.role}-role`}
                  value={r}
                  checked={role === r}
                  onChange={() => chooseRole(r)}
                />
                <span>{roleName(r)}</span>
              </label>
            ))}
          </fieldset>
          <p className="field-hint">
            {role === 'editor'
              ? 'An editor can type and resize. There is one editor at a time; a new editor link replaces the old one.'
              : 'A viewer watches the terminal and cannot type.'}
          </p>
          <div className="field">
            <label htmlFor={ids.label}>Label</label>
            <input
              id={ids.label}
              className="input"
              value={label}
              maxLength={80}
              onChange={(e) => setLabel(e.target.value)}
              placeholder="Who is this for?"
            />
          </div>
          <div className="field-row">
            <div className="field">
              <label htmlFor={ids.ttl}>Expires after</label>
              <select
                id={ids.ttl}
                className="input"
                value={ttl}
                onChange={(e) => setTtl(Number(e.target.value))}
              >
                {lifetimes.map((l) => (
                  <option key={l.seconds} value={l.seconds}>
                    {l.label}
                  </option>
                ))}
              </select>
            </div>
            <label className="check">
              <input
                id={ids.single}
                type="checkbox"
                checked={singleUse}
                onChange={(e) => setSingleUse(e.target.checked)}
              />
              <span>Single use</span>
            </label>
          </div>
          {error && <InlineError>{error}</InlineError>}
          <div>
            <button
              type="submit"
              className="btn btn--primary"
              disabled={pending}
            >
              <Link2 aria-hidden size={14} />{' '}
              {pending ? 'Creating…' : 'Create link'}
            </button>
          </div>
        </form>
      )}

      {grants.status === 'loading' && !grants.data && (
        <Loading label="Loading links" />
      )}
      {grants.status === 'error' && (
        <ErrorState
          error={grants.error}
          subject="terminal"
          onRetry={grants.reload}
        />
      )}
      {grants.data && <GrantList grants={grants.data} onRevoke={setRevoking} />}

      {confirmReplace && (
        <ConfirmDialog
          title="Replace the current editor?"
          confirmLabel="Replace editor"
          onConfirm={create}
          onCancel={() => setConfirmReplace(null)}
        >
          <p>
            The editor link <strong>{grantName(confirmReplace)}</strong> stops
            working and whoever is using it is disconnected. The new link
            becomes the only editor.
          </p>
        </ConfirmDialog>
      )}
      {revoking && (
        <ConfirmDialog
          title="Revoke this link?"
          confirmLabel="Revoke"
          onCancel={() => setRevoking(null)}
          onConfirm={async () => {
            await api.revokeGrant(terminalId, revoking.id)
            setRevoking(null)
            grants.reload()
          }}
        >
          <p>
            <strong>{grantName(revoking)}</strong> stops working, and anyone
            using it is disconnected immediately.
          </p>
        </ConfirmDialog>
      )}
    </section>
  )
}
