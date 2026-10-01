import type { ApiError } from '../api/client'

export interface ErrorCopy {
  title: string
  body: string
}

export function describeError(error: ApiError, subject = 'item'): ErrorCopy {
  switch (error.code) {
    case 'offline':
      return { title: 'The server could not be reached.', body: 'Check that webpty is still running and that this device is online, then try again.' }
    case 'untrusted_host':
      return {
        title: 'This address is not trusted by the server.',
        body: 'webpty only accepts admin requests on a loopback address unless WEBPTY_PUBLIC_ORIGIN is set. Restart webpty with WEBPTY_PUBLIC_ORIGIN set to the URL you open it at.',
      }
    case 'cross_origin':
      return { title: 'The request came from another site.', body: 'Open webpty directly at its own address and try again.' }
    case 'unauthenticated':
      return { title: 'You are signed out.', body: 'Sign in again to continue.' }
    case 'forbidden':
    case 'password_change_required':
    case 'csrf':
      return { title: 'You do not have access to this.', body: 'Reload the page. If this keeps happening, sign out and sign in again.' }
    case 'not_found':
      return { title: `This ${subject} was not found.`, body: 'It may have been removed, or the link may be wrong.' }
    case 'recording_deleted':
    case 'gone':
      return { title: 'This recording was deleted.', body: 'Its metadata is kept for the audit trail, but its events are gone.' }
    case 'recording_corrupt':
      return {
        title: 'This recording failed validation.',
        body: 'It was marked incomplete and cannot be played or exported. It will be removed with the other expired recordings.',
      }
    case 'recording_active':
      return { title: 'This recording is still in progress.', body: 'Playback and export become available when the terminal ends.' }
    case 'invalid_state':
      return { title: 'This terminal is not running.', body: 'It has already ended. Its recording, if any, is under Recordings.' }
    case 'session_limit':
      return { title: 'The running terminal limit is reached.', body: 'Terminate a terminal you no longer need, or raise WEBPTY_MAX_SESSIONS and restart webpty.' }
    case 'invalid_response':
      return { title: 'The server sent a response this page does not understand.', body: 'Reload the page. If you just upgraded webpty, clear the browser cache.' }
    case 'rate_limited':
      return { title: 'Too many attempts.', body: `Wait ${error.retryAfterSeconds ?? 60} seconds and try again.` }
    default:
      return { title: 'Something went wrong.', body: error.status >= 500 ? 'The server could not complete the request. Check the webpty log.' : error.message }
  }
}

