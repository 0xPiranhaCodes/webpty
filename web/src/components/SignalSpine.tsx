export type SpineState = 'idle' | 'live' | 'live-recording' | 'reconnecting' | 'ended' | 'lost' | 'playback'

/**
 * The workspace's state rail. Its colour and pattern mirror the text
 * readout next to it, so it is decorative for assistive technology.
 */
export function SignalSpine({ state }: { state: SpineState }) {
  return (
    <div className="spine" data-state={state} aria-hidden="true">
      <span className="spine__primary" />
      <span className="spine__secondary" />
    </div>
  )
}
