import { Check, Copy } from 'lucide-react'
import { useId, useRef, useState } from 'react'

export function CopyField({ label, value }: { label: string; value: string }) {
  const id = useId()
  const input = useRef<HTMLInputElement>(null)
  const [copied, setCopied] = useState<'yes' | 'manual' | null>(null)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied('yes')
    } catch {
      input.current?.select()
      setCopied('manual')
    }
  }

  return (
    <div className="copy-field">
      <label htmlFor={id}>{label}</label>
      <div className="copy-field__row">
        <input
          ref={input}
          id={id}
          className="input mono"
          readOnly
          value={value}
          onFocus={(e) => e.currentTarget.select()}
          spellCheck={false}
        />
        <button type="button" className="btn btn--primary" onClick={copy}>
          {copied === 'yes' ? (
            <Check aria-hidden size={14} />
          ) : (
            <Copy aria-hidden size={14} />
          )}
          {copied === 'yes' ? 'Copied' : 'Copy link'}
        </button>
      </div>
      <p className="field-hint" role="status">
        {copied === 'manual'
          ? 'Copying is blocked here. The link is selected; press Ctrl+C or ⌘C.'
          : ''}
      </p>
    </div>
  )
}
