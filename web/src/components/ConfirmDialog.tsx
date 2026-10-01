import { type ReactNode, useEffect, useId, useRef, useState } from 'react'

import { toApiError } from '../lib/useAsync'
import { describeError } from './describeError'
import { InlineError } from './StateViews'

export interface ConfirmDialogProps {
  title: string
  children: ReactNode
  confirmLabel: string
  onConfirm: () => Promise<unknown> | unknown
  onCancel: () => void
  destructive?: boolean
}

/**
 * A modal confirmation. Focus starts on Cancel so Enter never confirms a
 * destructive action by accident; Escape cancels; Tab stays inside.
 */
export function ConfirmDialog({ title, children, confirmLabel, onConfirm, onCancel, destructive = true }: ConfirmDialogProps) {
  const titleId = useId()
  const bodyId = useId()
  const panel = useRef<HTMLDivElement>(null)
  const cancel = useRef<HTMLButtonElement>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    cancel.current?.focus()
    return () => previous?.focus?.()
  }, [])

  const onKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === 'Escape' && !pending) {
      event.stopPropagation()
      onCancel()
    }
    if (event.key !== 'Tab' || !panel.current) return
    const focusable = [...panel.current.querySelectorAll<HTMLElement>('button:not([disabled])')]
    const first = focusable[0]
    const last = focusable.at(-1)
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last?.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first?.focus()
    }
  }

  const confirm = async () => {
    setPending(true)
    setError(null)
    try {
      await onConfirm()
    } catch (e) {
      const copy = describeError(toApiError(e))
      setError(`${copy.title} ${copy.body}`)
      setPending(false)
    }
  }

  return (
    <div className="dialog-backdrop" onKeyDown={onKeyDown}>
      <div ref={panel} className="dialog" role="alertdialog" aria-modal="true" aria-labelledby={titleId} aria-describedby={bodyId}>
        <h2 id={titleId}>{title}</h2>
        <div id={bodyId} className="dialog__body">
          {children}
        </div>
        {error && <InlineError>{error}</InlineError>}
        <div className="dialog__actions">
          <button ref={cancel} type="button" className="btn" onClick={onCancel} disabled={pending}>
            Cancel
          </button>
          <button type="button" className={destructive ? 'btn btn--destructive' : 'btn btn--primary'} onClick={confirm} disabled={pending}>
            {pending ? 'Working…' : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
