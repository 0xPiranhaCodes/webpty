import type { ConnectionState } from './connection'

export function describeState(state: ConnectionState): string {
  switch (state.kind) {
    case 'connecting':
      return state.attempt ? 'Reconnecting' : 'Connecting'
    case 'live':
      return 'Live'
    case 'reconnecting':
      return `Reconnecting in ${Math.max(1, Math.ceil(state.retryInMs / 1000))}s`
    case 'ended':
      switch (state.reason) {
        case 'exited': {
          const exit = state.exit
          if (exit?.state === 'terminated') return 'Terminated'
          if (exit?.signal) return `Ended by signal ${exit.signal}`
          if (exit?.exitCode != null)
            return `Ended with exit code ${exit.exitCode}`
          return 'Ended'
        }
        case 'revoked':
          return 'Access revoked'
        case 'replaced':
          return 'Access replaced by a new editor link'
        case 'superseded':
          return 'Replaced by a newer invitation in this browser'
        case 'expired':
          return 'Access expired'
        case 'logged_out':
          return 'Signed out'
        case 'unauthorized':
          return 'Signed out'
        case 'not_found':
          return 'Terminal not found'
        case 'closed':
          return 'Disconnected'
      }
  }
}
