import { type FormEvent, useId, useState } from 'react'

import { describeError } from '../components/describeError'
import { InlineError } from '../components/StateViews'
import { toApiError } from '../lib/useAsync'
import { useAdminAuth } from './authContext'
import { AuthFrame } from './AuthFrame'

function loginErrorMessage(error: unknown): string {
  const apiError = toApiError(error)
  switch (apiError.code) {
    case 'invalid_credentials':
      return 'That password is not correct.'
    case 'rate_limited':
      return `Too many attempts. Try again in ${apiError.retryAfterSeconds ?? 60} seconds.`
    default: {
      const copy = describeError(apiError)
      return `${copy.title} ${copy.body}`
    }
  }
}

export function LoginPage() {
  const auth = useAdminAuth()
  const id = useId()
  const [password, setPassword] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const serverError =
    auth.state.status === 'error' ? describeError(auth.state.error) : null
  const notice =
    auth.state.status === 'anonymous' ? auth.state.notice : undefined

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (!password) return
    setPending(true)
    setError(null)
    try {
      await auth.login(password)
    } catch (e) {
      setError(loginErrorMessage(e))
      setPassword('')
      setPending(false)
    }
  }

  return (
    <AuthFrame>
      <h1>Sign in</h1>
      <p className="muted lede">
        Administer terminals, shared links, and recordings on this machine.
      </p>
      {notice && (
        <p className="notice" role="status">
          {notice}
        </p>
      )}
      {serverError && (
        <InlineError>{`${serverError.title} ${serverError.body}`}</InlineError>
      )}
      <form className="form auth-form" onSubmit={submit} noValidate>
        <input
          type="text"
          name="username"
          autoComplete="username"
          value="webpty admin"
          readOnly
          hidden
        />
        <div className="field">
          <label htmlFor={id}>Administrator password</label>
          <input
            id={id}
            className="input"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            autoFocus
          />
          <p className="field-hint">
            First run? Use the initial password <code>CHANGEME</code>. You will
            replace it next.
          </p>
        </div>
        {error && <InlineError>{error}</InlineError>}
        <button
          type="submit"
          className="btn btn--primary"
          disabled={pending || !password}
        >
          {pending ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </AuthFrame>
  )
}
