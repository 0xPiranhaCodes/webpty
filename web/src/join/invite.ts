// Invitation tokens arrive in the URL fragment (/join#token=...). They are
// removed from the address bar before anything else runs and kept only in
// memory: never in storage, logs, errors, or rendered copy.

const tokenPattern = /^[A-Za-z0-9_-]{16,512}$/

let pending: string | null = null
let redemption: { token: string; promise: Promise<unknown> } | null = null

/** Removes a token fragment from the current URL and returns the token if well formed. */
export function captureInviteToken(
  loc: Location = window.location,
  hist: History = window.history,
): string | null {
  if (!loc.hash) return null
  const params = new URLSearchParams(loc.hash.slice(1))
  if (!params.has('token')) return null
  hist.replaceState(hist.state, '', loc.pathname + loc.search)
  const token = params.get('token') ?? ''
  return tokenPattern.test(token) ? token : null
}

/** Called once before the app renders so the token leaves the URL immediately. */
export function stashInviteFromLocation() {
  const token = captureInviteToken()
  if (token) pending = token
}

/** Returns the stashed token once. */
export function takePendingInvite(): string | null {
  const token = pending ?? captureInviteToken()
  pending = null
  return token
}

/**
 * Redeems a token at most once per page load, so a re-render or repeated
 * effect cannot spend a single-use invitation twice.
 */
export function redeemOnce<T>(
  token: string,
  redeem: (token: string) => Promise<T>,
): Promise<T> {
  if (redemption?.token !== token)
    redemption = { token, promise: redeem(token) }
  return redemption.promise as Promise<T>
}

let join: Promise<unknown> | null = null

/**
 * Starts joining once per page load: redeems a captured token, or resumes an
 * existing guest session when there is none. Repeated calls (StrictMode
 * effects, remounts) share the first attempt.
 */
export function beginJoin<T>(
  redeem: (token: string) => Promise<T>,
  resume: () => Promise<T>,
): Promise<T> {
  if (!join) {
    const token = takePendingInvite()
    join = token ? redeemOnce(token, redeem) : resume()
  }
  return join as Promise<T>
}

/** Forgets the join attempt after the guest leaves. */
export function endJoin() {
  join = null
}

export function resetInviteStateForTests() {
  pending = null
  redemption = null
  join = null
}
