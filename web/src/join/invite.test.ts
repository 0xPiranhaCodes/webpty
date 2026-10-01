import { beforeEach, describe, expect, test, vi } from 'vitest'

import { captureInviteToken, redeemOnce, resetInviteStateForTests, stashInviteFromLocation, takePendingInvite } from './invite'

const token = 'wpi_Z2l2ZS1tZS1hLXNlY3JldA'

beforeEach(() => {
  resetInviteStateForTests()
  window.history.replaceState(null, '', '/')
  localStorage.clear()
  sessionStorage.clear()
})

describe('invite token capture', () => {
  test('removes the token from the address bar without adding history', () => {
    window.history.replaceState({ keep: 1 }, '', `/join?from=chat#token=${token}`)
    const length = window.history.length

    expect(captureInviteToken()).toBe(token)

    expect(window.location.hash).toBe('')
    expect(window.location.href).not.toContain(token)
    expect(window.location.pathname + window.location.search).toBe('/join?from=chat')
    expect(window.history.length).toBe(length)
    expect(window.history.state).toEqual({ keep: 1 })
  })

  test('leaves unrelated fragments alone', () => {
    window.history.replaceState(null, '', '/join#section')
    expect(captureInviteToken()).toBeNull()
    expect(window.location.hash).toBe('#section')
  })

  test('scrubs malformed tokens but does not return them', () => {
    window.history.replaceState(null, '', '/join#token=%3Cscript%3E')
    expect(captureInviteToken()).toBeNull()
    expect(window.location.hash).toBe('')
  })

  test('the stashed token can be taken once and is never persisted', () => {
    window.history.replaceState(null, '', `/join#token=${token}`)
    stashInviteFromLocation()

    expect(window.location.hash).toBe('')
    expect(takePendingInvite()).toBe(token)
    expect(takePendingInvite()).toBeNull()
    expect(JSON.stringify({ ...localStorage })).not.toContain(token)
    expect(JSON.stringify({ ...sessionStorage })).not.toContain(token)
    expect(document.cookie).not.toContain(token)
  })
})

describe('redeemOnce', () => {
  test('issues one redemption per token even when called repeatedly', async () => {
    const redeem = vi.fn(async (t: string) => ({ ok: t.length }))
    const [a, b] = await Promise.all([redeemOnce(token, redeem), redeemOnce(token, redeem)])
    expect(redeem).toHaveBeenCalledTimes(1)
    expect(a).toBe(b)
  })

  test('a failed redemption is shared, not retried with the same single-use token', async () => {
    const redeem = vi.fn(async () => {
      throw new Error('invalid or expired invitation')
    })
    await expect(redeemOnce(token, redeem)).rejects.toThrow('invalid or expired invitation')
    await expect(redeemOnce(token, redeem)).rejects.toThrow('invalid or expired invitation')
    expect(redeem).toHaveBeenCalledTimes(1)
  })
})
