import { Check, Minus } from 'lucide-react'
import { type FormEvent, useId, useState } from 'react'

import { describeError } from '../components/describeError'
import { InlineError } from '../components/StateViews'
import { toApiError } from '../lib/useAsync'
import { useAdminAuth } from './authContext'
import { AuthFrame } from './AuthFrame'
import { passwordRules, validateNewPassword } from './password'

export function ChangePasswordPage() {
  const auth = useAdminAuth()
  const ids = {
    current: useId(),
    next: useId(),
    confirm: useId(),
    rules: useId(),
  }
  const askCurrent = !auth.knowsBootstrapPassword
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const problem = validateNewPassword(next, confirm)
    if (problem) {
      setError(problem)
      return
    }
    setPending(true)
    setError(null)
    try {
      await auth.changePassword(askCurrent ? current : undefined, next)
    } catch (e) {
      const apiError = toApiError(e)
      if (apiError.code === 'invalid_current_password')
        setError('The current password is not correct.')
      else if (apiError.status === 400)
        setError(
          'The server rejected this password. Use at least 12 characters, not the default.',
        )
      else {
        const copy = describeError(apiError)
        setError(`${copy.title} ${copy.body}`)
      }
      setPending(false)
    }
  }

  return (
    <AuthFrame>
      <h1>Set the administrator password</h1>
      <p className="muted lede">
        webpty is still using its initial password. Replace it before managing
        terminals; anyone who knows the default could otherwise run commands on
        this machine.
      </p>
      <form className="form auth-form" onSubmit={submit} noValidate>
        <input
          type="text"
          name="username"
          autoComplete="username"
          value="webpty admin"
          readOnly
          hidden
        />
        {askCurrent && (
          <div className="field">
            <label htmlFor={ids.current}>Current password</label>
            <input
              id={ids.current}
              className="input"
              type="password"
              autoComplete="current-password"
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
              required
            />
          </div>
        )}
        <div className="field">
          <label htmlFor={ids.next}>New password</label>
          <input
            id={ids.next}
            className="input"
            type="password"
            autoComplete="new-password"
            aria-describedby={ids.rules}
            value={next}
            onChange={(e) => setNext(e.target.value)}
            required
            autoFocus={!askCurrent}
          />
        </div>
        <div className="field">
          <label htmlFor={ids.confirm}>Confirm new password</label>
          <input
            id={ids.confirm}
            className="input"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            required
          />
        </div>
        <ul className="rules" id={ids.rules} aria-label="Password requirements">
          {passwordRules(next, confirm).map((rule) => (
            <li key={rule.label} data-met={rule.met}>
              {rule.met ? (
                <Check aria-hidden size={14} />
              ) : (
                <Minus aria-hidden size={14} />
              )}
              <span>{rule.label}</span>
              <span className="visually-hidden">
                {rule.met ? ' (met)' : ' (not met)'}
              </span>
            </li>
          ))}
        </ul>
        {error && <InlineError>{error}</InlineError>}
        <div className="form__actions">
          <button type="submit" className="btn btn--primary" disabled={pending}>
            {pending ? 'Saving…' : 'Save password'}
          </button>
          <button
            type="button"
            className="btn btn--quiet"
            onClick={() => void auth.logout()}
          >
            Sign out
          </button>
        </div>
      </form>
    </AuthFrame>
  )
}
