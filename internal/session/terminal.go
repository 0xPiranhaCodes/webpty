package session

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

var errNotRunning = fmt.Errorf("%w: session is not running", ErrInvalidState)

// terminal is one live session. It owns exactly one process and PTY.
type terminal struct {
	id   string
	m    *Manager
	proc pty.Process
	done chan struct{}

	// input feeds inputLoop, the only goroutine that writes to the PTY, so
	// a child that never reads stdin cannot block callers past their ctx.
	input chan inputRequest
	// inputClosed is closed once the PTY is closed and inputLoop exits.
	inputClosed chan struct{}
	// forced is closed by forceKill.
	forced    chan struct{}
	forceOnce sync.Once

	mu          sync.Mutex
	info        Info
	seq         uint64
	replay      replayBuffer
	subs        map[*Subscription]struct{}
	detachedAt  time.Time
	idleTimer   Timer
	idleGen     uint64
	killTimer   Timer
	terminating bool
	termReason  string
	termRemote  string
	finished    bool
	// rec is cleared once it has received the final lifecycle event.
	rec Recorder
}

func newTerminal(m *Manager, info Info, proc pty.Process) *terminal {
	return &terminal{
		id:          info.PublicID,
		m:           m,
		proc:        proc,
		done:        make(chan struct{}),
		input:       make(chan inputRequest),
		inputClosed: make(chan struct{}),
		forced:      make(chan struct{}),
		info:        info,
		replay:      replayBuffer{limit: m.cfg.ReplayBytes},
		subs:        map[*Subscription]struct{}{},
	}
}

type inputRequest struct {
	data []byte
	// allow, if set, is checked immediately before the input is written.
	allow func() bool
	done  chan error
}

func (t *terminal) start() {
	t.mu.Lock()
	t.detachedAt = t.m.cfg.Now()
	t.armIdleLocked()
	t.mu.Unlock()
	go t.inputLoop()
	go t.run()
}

func (t *terminal) snapshot() Info {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.info
}

// run reads output until the process is reaped, then releases the PTY.
func (t *terminal) run() {
	readerDone := make(chan struct{})
	go t.readLoop(readerDone)
	status, waitErr := t.proc.Wait()

	drain := time.NewTimer(t.m.cfg.DrainTimeout)
	select {
	case <-readerDone:
	case <-drain.C:
		t.m.cfg.Logger.Warn("terminal output drain timed out", "sessionId", t.id)
	case <-t.forced:
	}
	drain.Stop()
	if err := t.proc.Close(); err != nil {
		t.m.cfg.Logger.Warn("close terminal pty", "sessionId", t.id, "error", err)
	}
	// Closing the PTY fails any write in progress, so inputLoop can exit.
	close(t.inputClosed)
	t.finish(status, waitErr)
}

func (t *terminal) inputLoop() {
	for {
		select {
		case req := <-t.input:
			if req.allow != nil && !req.allow() {
				req.done <- ErrInputRevoked
				continue
			}
			req.done <- t.writeAll(req.data)
		case <-t.inputClosed:
			return
		}
	}
}

func (t *terminal) writeAll(data []byte) error {
	for len(data) > 0 {
		n, err := t.proc.Write(data)
		if err != nil {
			return fmt.Errorf("%w: write input: %v", ErrProcessFailed, err)
		}
		data = data[n:]
	}
	return nil
}

func (t *terminal) readLoop(done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, readChunk)
	for {
		n, err := t.proc.Read(buf)
		if n > 0 {
			t.publish(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// publish fans data out without blocking; viewers that cannot keep up are
// disconnected.
func (t *terminal) publish(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	event := Event{Kind: EventOutput, Seq: t.seq, Data: data}
	t.replay.add(event)
	if t.rec != nil {
		t.rec.Output(data)
	}
	t.info.LastActivityAt = t.m.cfg.Now()
	t.fanOutLocked(event)
}

func (t *terminal) fanOutLocked(event Event) {
	dropped := false
	for sub := range t.subs {
		if !sub.push(event) {
			delete(t.subs, sub)
			dropped = true
			t.m.cfg.Logger.Warn("terminal viewer disconnected: slow consumer",
				"sessionId", t.id, "queueLimitBytes", sub.limit)
		}
	}
	if dropped && len(t.subs) == 0 {
		t.detachedAt = t.m.cfg.Now()
		t.armIdleLocked()
	}
}

func (t *terminal) subscribe(opts SubscribeOptions) (*Subscription, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished || t.info.State != store.TerminalRunning {
		return nil, fmt.Errorf("%w: session is not running", ErrInvalidState)
	}
	if len(t.subs) >= t.m.cfg.MaxViewers {
		return nil, ErrViewerLimit
	}
	events, err := t.replay.after(opts.Resume, opts.AfterSeq, t.seq)
	if err != nil {
		return nil, err
	}
	sub := &Subscription{
		terminal: t,
		info:     t.info,
		lastSeq:  t.seq,
		limit:    t.m.cfg.ClientQueueBytes,
		notify:   make(chan struct{}, 1),
	}
	for _, event := range events {
		sub.push(event)
	}
	t.subs[sub] = struct{}{}
	t.stopIdleLocked()
	return sub, nil
}

func (t *terminal) detach(sub *Subscription) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.subs[sub]; !ok {
		return
	}
	delete(t.subs, sub)
	if len(t.subs) == 0 {
		t.detachedAt = t.m.cfg.Now()
		t.armIdleLocked()
	}
}

func (t *terminal) write(ctx context.Context, data []byte, allow func() bool) error {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return errNotRunning
	}
	t.info.LastActivityAt = t.m.cfg.Now()
	t.mu.Unlock()

	req := inputRequest{data: data, allow: allow, done: make(chan error, 1)}
	select {
	case t.input <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.inputClosed:
		return errNotRunning
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.inputClosed:
		return errNotRunning
	}
}

func (t *terminal) resize(size pty.Size) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return fmt.Errorf("%w: session is not running", ErrInvalidState)
	}
	if t.info.Rows == int(size.Rows) && t.info.Cols == int(size.Cols) {
		return nil
	}
	if err := t.proc.Resize(size); err != nil {
		return fmt.Errorf("%w: resize: %v", ErrProcessFailed, err)
	}
	t.info.Rows, t.info.Cols = int(size.Rows), int(size.Cols)
	t.info.LastActivityAt = t.m.cfg.Now()
	if t.rec != nil {
		t.rec.Resize(t.info.Rows, t.info.Cols)
	}
	t.fanOutLocked(Event{Kind: EventResize, Rows: t.info.Rows, Cols: t.info.Cols})
	return nil
}

func (t *terminal) terminate(reason, remoteAddr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.terminateLocked(reason, remoteAddr)
}

// terminateLocked sends SIGTERM to the process group and escalates to
// SIGKILL after the grace period. Only the first request has an effect.
func (t *terminal) terminateLocked(reason, remoteAddr string) {
	if t.finished || t.terminating {
		return
	}
	t.terminating, t.termReason, t.termRemote = true, reason, remoteAddr
	t.stopIdleLocked()
	t.m.cfg.Logger.Info("terminal session terminating", "sessionId", t.id, "reason", reason)
	if err := t.proc.SignalGroup(syscall.SIGTERM); err != nil {
		t.m.cfg.Logger.Warn("signal terminal process group", "sessionId", t.id, "signal", "SIGTERM", "error", err)
	}
	t.killTimer = t.m.cfg.AfterFunc(t.m.cfg.KillGrace, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.finished {
			return
		}
		t.m.cfg.Logger.Warn("terminal session ignored SIGTERM; killing", "sessionId", t.id)
		if err := t.proc.SignalGroup(syscall.SIGKILL); err != nil {
			t.m.cfg.Logger.Warn("signal terminal process group", "sessionId", t.id, "signal", "SIGKILL", "error", err)
		}
	})
}

// forceKill SIGKILLs the process group now and stops waiting for output to
// drain once it is reaped.
func (t *terminal) forceKill() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.terminateLocked("shutdown", "")
	if t.killTimer != nil {
		t.killTimer.Stop()
	}
	if err := t.proc.SignalGroup(syscall.SIGKILL); err != nil {
		t.m.cfg.Logger.Warn("signal terminal process group", "sessionId", t.id, "signal", "SIGKILL", "error", err)
	}
	t.forceOnce.Do(func() { close(t.forced) })
}

func (t *terminal) armIdleLocked() {
	idle := t.m.cfg.IdleTimeout
	if idle <= 0 || t.finished || t.terminating || len(t.subs) > 0 || t.idleTimer != nil {
		return
	}
	since := t.detachedAt
	if t.info.LastActivityAt.After(since) {
		since = t.info.LastActivityAt
	}
	remaining := idle - t.m.cfg.Now().Sub(since)
	if remaining < 0 {
		remaining = 0
	}
	t.idleGen++
	gen := t.idleGen
	t.idleTimer = t.m.cfg.AfterFunc(remaining, func() { t.idleFired(gen) })
}

func (t *terminal) stopIdleLocked() {
	if t.idleTimer != nil {
		t.idleTimer.Stop()
		t.idleTimer = nil
	}
	t.idleGen++
}

func (t *terminal) idleFired(gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.idleGen {
		return
	}
	t.idleTimer = nil
	if t.finished || t.terminating || len(t.subs) > 0 {
		return
	}
	since := t.detachedAt
	if t.info.LastActivityAt.After(since) {
		since = t.info.LastActivityAt
	}
	if t.m.cfg.Now().Sub(since) >= t.m.cfg.IdleTimeout {
		t.terminateLocked("idle", "")
		return
	}
	t.armIdleLocked()
}

// finish records and audits the final state, then notifies viewers and
// releases the slot.
func (t *terminal) finish(status pty.ExitStatus, waitErr error) {
	t.mu.Lock()
	t.finished = true
	t.stopIdleLocked()
	if t.killTimer != nil {
		t.killTimer.Stop()
	}
	final := t.info
	info := &final
	info.EndedAt = t.m.cfg.Now()
	info.ExitSignal = status.Signal
	switch {
	case waitErr != nil:
		info.State = store.TerminalFailed
		info.Failure = "wait failed: " + waitErr.Error()
	case t.terminating:
		info.State = store.TerminalTerminated
	default:
		info.State = store.TerminalExited
	}
	if waitErr == nil && status.Signal == "" {
		code := status.Code
		info.ExitCode = &code
	}
	exit := Exit{State: info.State, Code: info.ExitCode, Signal: info.ExitSignal}
	subs := make([]*Subscription, 0, len(t.subs))
	for sub := range t.subs {
		subs = append(subs, sub)
	}
	clear(t.subs)
	reason, remote := t.termReason, t.termRemote
	if t.rec != nil {
		end := Lifecycle{State: info.State, ExitCode: info.ExitCode, Signal: info.ExitSignal}
		switch info.State {
		case store.TerminalFailed:
			end.Reason = "wait_failed"
		case store.TerminalTerminated:
			end.Reason = reason
		}
		t.rec.End(end)
		t.rec = nil
	}
	t.mu.Unlock()

	t.m.persistFinal(final)
	details := map[string]string{"sessionId": final.PublicID}
	if final.ExitCode != nil {
		details["exitCode"] = strconv.Itoa(*final.ExitCode)
	}
	if final.ExitSignal != "" {
		details["signal"] = final.ExitSignal
	}
	logger := t.m.cfg.Logger
	switch final.State {
	case store.TerminalFailed:
		details["reason"] = "wait_failed"
		t.m.audit("terminal.session.failed", "", details)
		logger.Error("terminal session failed", "sessionId", final.PublicID, "error", waitErr)
	case store.TerminalTerminated:
		details["reason"] = reason
		t.m.audit("terminal.session.terminated", remote, details)
		logger.Info("terminal session terminated", "sessionId", final.PublicID, "reason", reason, "signal", final.ExitSignal)
	default:
		t.m.audit("terminal.session.exited", "", details)
		logger.Info("terminal session exited", "sessionId", final.PublicID, "exitCode", details["exitCode"], "signal", final.ExitSignal)
	}
	// Readers and viewers see the final state once it is durable, or once
	// the Manager holds it for a later retry.
	t.mu.Lock()
	t.info = final
	t.mu.Unlock()
	for _, sub := range subs {
		sub.finish(exit)
	}
	if t.m.cfg.OnEnd != nil {
		t.m.cfg.OnEnd(final.PublicID)
	}
	t.m.remove(final.PublicID)
	close(t.done)
}
