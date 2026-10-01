package session

import "github.com/0xPiranhaCodes/webpty/internal/store"

// Recorder receives one session's terminal output, size, and lifecycle
// events in order. It never receives terminal input. Its methods are called
// while the session's lock is held, so they must return without blocking.
type Recorder interface {
	// Output records PTY output. The session never modifies data after the
	// call, so the recorder may retain it but must not modify it.
	Output(data []byte)
	Resize(rows, cols int)
	Lifecycle(Lifecycle)
	// End records the final lifecycle event. No calls follow it.
	End(Lifecycle)
}

// Recorders begins session recordings.
type Recorders interface {
	// Begin is called once the session is persisted and before its process
	// starts, so no output precedes it. A nil Recorder leaves the session
	// unrecorded.
	Begin(info Info) Recorder
}

// Lifecycle is a recorded state change. Reason is a fixed code such as
// "admin", "idle", "shutdown", "start_failed", or "wait_failed"; it never
// carries addresses, commands, or error text.
type Lifecycle struct {
	State    store.TerminalState
	ExitCode *int
	Signal   string
	Reason   string
}
