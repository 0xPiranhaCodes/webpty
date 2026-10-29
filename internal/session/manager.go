// Package session owns PTY terminal sessions: lifecycle, limits, bounded
// output replay, and fan-out to viewers.
package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// Size and command limits.
const (
	MaxRows         = 1000
	MaxCols         = 1000
	MaxArgs         = 256
	MaxCommandBytes = 32 << 10

	defaultRows = 24
	defaultCols = 80
	readChunk   = 16 << 10

	persistTimeout = 5 * time.Second
)

// Defaults for zero Config fields; they match the config package's.
const (
	defaultCommandPath      = "/bin/sh"
	defaultMaxSessions      = 16
	defaultMaxViewers       = 8
	defaultKillGrace        = 5 * time.Second
	defaultDrainTimeout     = 2 * time.Second
	defaultForceReapTimeout = 5 * time.Second
	defaultReplayBytes      = 256 << 10
	defaultClientQueueBytes = 1 << 20
)

// Info is a session's metadata.
type Info = store.TerminalSession

// Repository persists session metadata and audit events.
type Repository interface {
	CreateTerminalSession(context.Context, store.TerminalSession) error
	UpdateTerminalSession(context.Context, store.TerminalSession) error
	TerminalSession(context.Context, string) (store.TerminalSession, error)
	TerminalSessions(context.Context) ([]store.TerminalSession, error)
	FailInterruptedTerminalSessions(context.Context, time.Time) (int64, error)
	AppendAuditEvent(context.Context, store.AuditEvent) error
}

// Timer is a stoppable pending callback.
type Timer interface {
	Stop() bool
}

// Config wires a Manager. Zero numeric fields receive defaults; a zero
// IdleTimeout disables idle termination.
type Config struct {
	Store          Repository
	Starter        pty.Starter
	DefaultCommand pty.Command
	// CommandPolicy restricts the executables sessions may run. Nil means
	// AllowAllCommands.
	CommandPolicy *CommandPolicy

	MaxSessions int
	MaxViewers  int
	IdleTimeout time.Duration
	// KillGrace is how long a terminated process group has between SIGTERM
	// and SIGKILL.
	KillGrace time.Duration
	// DrainTimeout bounds how long output is read after the process exits.
	DrainTimeout time.Duration
	// ForceReapTimeout bounds how long Close waits for sessions to be
	// reaped after it SIGKILLs them.
	ForceReapTimeout time.Duration
	ReplayBytes      int
	ClientQueueBytes int

	// OnEnd, if set, is called once per session after its final state is
	// persisted and its viewers have been told it ended.
	OnEnd func(id string)

	// Recorders, if set, records sessions not created with NoRecording.
	Recorders Recorders

	Logger    *slog.Logger
	Now       func() time.Time
	AfterFunc func(time.Duration, func()) Timer
}

// Manager owns every live session.
type Manager struct {
	cfg Config

	mu       sync.Mutex
	live     map[string]*terminal
	starting int
	closed   bool
	// forced is set once Close stops waiting for a graceful exit; sessions
	// that finish starting afterwards are killed at once.
	forced bool
	// unpersisted holds final states the store rejected, so readers never
	// see a phantom running session; they are retried on later reads.
	unpersisted map[string]Info
	// retrying is set while a retry of unpersisted is in flight; one that
	// is stuck in the store is never joined by another.
	retrying bool
	wg       sync.WaitGroup
}

// NewManager validates cfg and marks sessions orphaned by a previous server
// process as failed.
func NewManager(ctx context.Context, cfg Config) (*Manager, error) {
	cfg, err := withManagerDefaults(cfg)
	if err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, live: map[string]*terminal{}, unpersisted: map[string]Info{}}
	count, err := cfg.Store.FailInterruptedTerminalSessions(ctx, cfg.Now())
	if err != nil {
		return nil, err
	}
	if count > 0 {
		cfg.Logger.Warn("terminal sessions interrupted by restart marked failed", "count", count)
	}
	return m, nil
}

func withManagerDefaults(cfg Config) (Config, error) {
	if cfg.Store == nil || cfg.Starter == nil {
		return Config{}, errors.New("session: store and starter are required")
	}
	if cfg.DefaultCommand.Path == "" {
		cfg.DefaultCommand.Path = defaultCommandPath
	}
	if cfg.CommandPolicy == nil {
		cfg.CommandPolicy = AllowAllCommands()
	}
	setDefault(&cfg.MaxSessions, defaultMaxSessions)
	setDefault(&cfg.MaxViewers, defaultMaxViewers)
	setDefault(&cfg.KillGrace, defaultKillGrace)
	setDefault(&cfg.DrainTimeout, defaultDrainTimeout)
	setDefault(&cfg.ForceReapTimeout, defaultForceReapTimeout)
	setDefault(&cfg.ReplayBytes, defaultReplayBytes)
	setDefault(&cfg.ClientQueueBytes, defaultClientQueueBytes)
	if err := checkLimits(cfg); err != nil {
		return Config{}, err
	}
	setRuntimeDefaults(&cfg)
	return cfg, nil
}

func checkLimits(cfg Config) error {
	if cfg.MaxSessions < 0 || cfg.MaxViewers < 0 || cfg.ReplayBytes < 0 || cfg.ClientQueueBytes < 0 || cfg.IdleTimeout < 0 {
		return errors.New("session: limits must not be negative")
	}
	if cfg.ClientQueueBytes < cfg.ReplayBytes {
		return fmt.Errorf("session: client queue (%d bytes) must hold the replay buffer (%d bytes)",
			cfg.ClientQueueBytes, cfg.ReplayBytes)
	}
	return nil
}

// setRuntimeDefaults fills the injectable logger, clock, and timer.
func setRuntimeDefaults(cfg *Config) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
}

func setDefault[T int | time.Duration](v *T, fallback T) {
	if *v == 0 {
		*v = fallback
	}
}

// CreateRequest describes a new session. A zero Command uses the default
// command; zero Rows or Cols use 24x80.
type CreateRequest struct {
	Command    pty.Command
	Rows, Cols int
	RemoteAddr string
	// NoRecording opts the session out of recording.
	NoRecording bool
}

// Create starts a session and returns it running, or returns
// ErrProcessFailed after recording the failed session.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (Info, error) {
	command, size, err := m.normalize(req)
	if err != nil {
		return Info{}, err
	}
	if !m.cfg.CommandPolicy.Permits(command.Path) {
		m.audit("terminal.session.denied", req.RemoteAddr, map[string]string{"command": command.Path, "reason": "command_policy"})
		m.cfg.Logger.Warn("terminal command denied by policy", "command", command.Path)
		return Info{}, ErrCommandDenied
	}
	id, err := newID()
	if err != nil {
		return Info{}, err
	}
	if err := m.reserveStart(); err != nil {
		return Info{}, err
	}
	info, err := m.createRecord(ctx, req, id, command, size)
	if err != nil {
		m.releaseStart()
		return Info{}, err
	}
	return m.launch(req, info, command, size)
}

// reserveStart counts a session as starting, so the session limit and
// Close account for it before its process exists.
func (m *Manager) reserveStart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.closed || m.live == nil:
		return ErrClosed
	case len(m.live)+m.starting >= m.cfg.MaxSessions:
		return ErrSessionLimit
	}
	m.starting++
	m.wg.Add(1)
	return nil
}

// releaseStart undoes reserveStart for a session that never went live.
func (m *Manager) releaseStart() {
	m.mu.Lock()
	m.starting--
	m.mu.Unlock()
	m.wg.Done()
}

// createRecord persists the starting session and audits its creation.
// Only the argument count is audited, never the arguments themselves.
func (m *Manager) createRecord(ctx context.Context, req CreateRequest, id string, command pty.Command, size pty.Size) (Info, error) {
	now := m.cfg.Now()
	info := Info{
		PublicID: id, State: store.TerminalStarting, Command: command.Path, Args: command.Args,
		Rows: int(size.Rows), Cols: int(size.Cols), CreatedAt: now, LastActivityAt: now,
	}
	if err := m.cfg.Store.CreateTerminalSession(ctx, info); err != nil {
		return Info{}, err
	}
	m.audit("terminal.session.created", req.RemoteAddr, map[string]string{
		"sessionId": id, "command": command.Path, "argCount": strconv.Itoa(len(command.Args)),
		"rows": strconv.Itoa(info.Rows), "cols": strconv.Itoa(info.Cols),
	})
	m.cfg.Logger.Info("terminal session created", "sessionId", id, "command", command.Path, "argCount", len(command.Args))
	return info, nil
}

// launch starts the process and moves the reserved session into live.
func (m *Manager) launch(req CreateRequest, info Info, command pty.Command, size pty.Size) (Info, error) {
	var rec Recorder
	if m.cfg.Recorders != nil && !req.NoRecording {
		rec = m.cfg.Recorders.Begin(info)
	}
	proc, err := m.cfg.Starter.Start(command, size)
	if err != nil {
		m.failStart(req, info, rec, err)
		m.releaseStart()
		return Info{}, fmt.Errorf("%w: %v", ErrProcessFailed, err)
	}
	info.State = store.TerminalRunning
	info.StartedAt = m.cfg.Now()
	m.persist(info)
	m.audit("terminal.session.started", req.RemoteAddr, map[string]string{"sessionId": info.PublicID, "pid": strconv.Itoa(proc.Pid())})
	m.cfg.Logger.Info("terminal session started", "sessionId", info.PublicID, "pid", proc.Pid())
	if rec != nil {
		rec.Lifecycle(Lifecycle{State: store.TerminalRunning})
	}
	t := newTerminal(m, info, proc)
	t.rec = rec
	return m.goLive(t), nil
}

func (m *Manager) failStart(req CreateRequest, info Info, rec Recorder, cause error) {
	info.State = store.TerminalFailed
	info.EndedAt = m.cfg.Now()
	info.Failure = "start failed: " + cause.Error()
	if rec != nil {
		rec.End(Lifecycle{State: store.TerminalFailed, Reason: "start_failed"})
	}
	m.persist(info)
	m.audit("terminal.session.failed", req.RemoteAddr, map[string]string{"sessionId": info.PublicID, "reason": "start_failed"})
	m.cfg.Logger.Warn("terminal session failed to start", "sessionId", info.PublicID, "command", info.Command, "error", cause)
}

// goLive publishes t and starts it. A Close that began while t was
// starting is applied to it at once.
func (m *Manager) goLive(t *terminal) Info {
	m.mu.Lock()
	m.starting--
	m.live[t.info.PublicID] = t
	closed, forced := m.closed, m.forced
	m.mu.Unlock()
	t.start()
	switch {
	case forced:
		t.forceKill()
	case closed:
		t.terminate("shutdown", "")
	}
	return t.snapshot()
}

func (m *Manager) normalize(req CreateRequest) (pty.Command, pty.Size, error) {
	command := req.Command
	if command.Path == "" {
		if len(command.Args) > 0 {
			return pty.Command{}, pty.Size{}, invalid("arguments require a command")
		}
		command = m.cfg.DefaultCommand
	}
	if len(command.Args) > MaxArgs {
		return pty.Command{}, pty.Size{}, invalid("at most %d arguments", MaxArgs)
	}
	total := 0
	for _, part := range append([]string{command.Path}, command.Args...) {
		if strings.IndexByte(part, 0) >= 0 {
			return pty.Command{}, pty.Size{}, invalid("command contains a NUL byte")
		}
		total += len(part)
	}
	if total > MaxCommandBytes {
		return pty.Command{}, pty.Size{}, invalid("command exceeds %d bytes", MaxCommandBytes)
	}
	command.Args = append([]string{}, command.Args...)

	rows, cols := req.Rows, req.Cols
	if rows == 0 {
		rows = defaultRows
	}
	if cols == 0 {
		cols = defaultCols
	}
	size, err := checkSize(rows, cols)
	return command, size, err
}

func checkSize(rows, cols int) (pty.Size, error) {
	if rows < 1 || rows > MaxRows || cols < 1 || cols > MaxCols {
		return pty.Size{}, invalid("size must be 1..%d rows and 1..%d columns", MaxRows, MaxCols)
	}
	return pty.Size{Rows: uint16(rows), Cols: uint16(cols)}, nil
}

// Get returns a session's metadata.
func (m *Manager) Get(ctx context.Context, id string) (Info, error) {
	if t := m.lookup(id); t != nil {
		return t.snapshot(), nil
	}
	m.retryUnpersisted(ctx)
	if info, ok := m.pendingFinal(id); ok {
		return info, nil
	}
	return m.stored(ctx, id)
}

// List returns every known session in creation order.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	m.retryUnpersisted(ctx)
	sessions, err := m.cfg.Store.TerminalSessions(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		if t := m.lookup(sessions[i].PublicID); t != nil {
			sessions[i] = t.snapshot()
		} else if info, ok := m.pendingFinal(sessions[i].PublicID); ok {
			sessions[i] = info
		}
	}
	return sessions, nil
}

// Write forwards terminal input through the session's single input writer.
// It returns ctx.Err() if the PTY does not accept the input before ctx is
// done; input already handed to the writer may still be delivered later.
func (m *Manager) Write(ctx context.Context, id string, data []byte) error {
	t, err := m.running(ctx, id)
	if err != nil {
		return err
	}
	return t.write(ctx, data, nil)
}

// WriteAuthorized is Write, except allow is called immediately before the
// input reaches the PTY and ErrInputRevoked is returned if it reports false.
// Input queued behind a slow write therefore cannot outlive a revocation.
func (m *Manager) WriteAuthorized(ctx context.Context, id string, data []byte, allow func() bool) error {
	t, err := m.running(ctx, id)
	if err != nil {
		return err
	}
	return t.write(ctx, data, allow)
}

// Resize changes the terminal size.
func (m *Manager) Resize(id string, rows, cols int) error {
	size, err := checkSize(rows, cols)
	if err != nil {
		return err
	}
	t, err := m.running(context.Background(), id)
	if err != nil {
		return err
	}
	return t.resize(size)
}

// Terminate ends a session and waits until its process is reaped or ctx is
// done. Terminating an ended session returns its final metadata.
func (m *Manager) Terminate(ctx context.Context, id, remoteAddr string) (Info, error) {
	t := m.lookup(id)
	if t == nil {
		info, err := m.stored(ctx, id)
		if err == nil && info.State == store.TerminalStarting {
			return info, fmt.Errorf("%w: session is starting", ErrInvalidState)
		}
		return info, err
	}
	t.terminate("admin", remoteAddr)
	select {
	case <-t.done:
		return t.snapshot(), nil
	case <-ctx.Done():
		return t.snapshot(), ctx.Err()
	}
}

// SubscribeOptions selects replayed output. Without Resume every buffered
// event is replayed; with Resume only events after AfterSeq are, or
// ErrReplayGap is returned if some are no longer buffered.
type SubscribeOptions struct {
	Resume   bool
	AfterSeq uint64
}

// Subscribe attaches a viewer to a running session.
func (m *Manager) Subscribe(ctx context.Context, id string, opts SubscribeOptions) (*Subscription, error) {
	t, err := m.running(ctx, id)
	if err != nil {
		return nil, err
	}
	return t.subscribe(opts)
}

// Close terminates every session and waits until all are reaped. Sessions
// still running when ctx is done have their process groups SIGKILLed, and
// Close waits up to Config.ForceReapTimeout more for them to be reaped; it
// returns an error only if some are not. Later calls to Create fail with
// ErrClosed. It is idempotent.
func (m *Manager) Close(ctx context.Context) error {
	if m.live == nil {
		return nil
	}
	// Bounded by persistTimeout, so a wedged store cannot hold up shutdown.
	defer m.retryUnpersisted(context.Background())
	for _, t := range m.markClosed(false) {
		t.terminate("shutdown", "")
	}
	done := goWait(&m.wg)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	terminals := m.markClosed(true)
	m.cfg.Logger.Warn("terminal shutdown deadline passed; killing remaining sessions", "sessions", len(terminals))
	for _, t := range terminals {
		t.forceKill()
	}
	return m.awaitReap(ctx, done)
}

// markClosed sets closed, or forced as well, and returns the live sessions
// at that moment; sessions that start later see the flag in goLive.
func (m *Manager) markClosed(forced bool) []*terminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if forced {
		m.forced = true
	}
	return m.liveLocked()
}

// awaitReap waits up to ForceReapTimeout for SIGKILLed sessions to be reaped.
func (m *Manager) awaitReap(ctx context.Context, done <-chan struct{}) error {
	reap := time.NewTimer(m.cfg.ForceReapTimeout)
	defer reap.Stop()
	select {
	case <-done:
		return nil
	case <-reap.C:
		m.mu.Lock()
		remaining := len(m.live)
		m.mu.Unlock()
		return fmt.Errorf("session: %d sessions not reaped after SIGKILL: %w", remaining, ctx.Err())
	}
}

// goWait returns a channel closed once wg's counter reaches zero.
func goWait(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

func (m *Manager) liveLocked() []*terminal {
	terminals := make([]*terminal, 0, len(m.live))
	for _, t := range m.live {
		terminals = append(terminals, t)
	}
	return terminals
}

func (m *Manager) lookup(id string) *terminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[id]
}

// running returns the live terminal for id, or ErrInvalidState if the
// session exists but is not running.
func (m *Manager) running(ctx context.Context, id string) (*terminal, error) {
	if t := m.lookup(id); t != nil {
		return t, nil
	}
	if _, err := m.stored(ctx, id); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: session is not running", ErrInvalidState)
}

func (m *Manager) stored(ctx context.Context, id string) (Info, error) {
	info, err := m.cfg.Store.TerminalSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Info{}, ErrNotFound
	}
	return info, err
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.live, id)
	m.mu.Unlock()
	m.wg.Done()
}

// persistFinal records a session's final state, keeping it in memory for
// retry if the store rejects it.
func (m *Manager) persistFinal(info Info) {
	if m.persist(info) {
		return
	}
	m.mu.Lock()
	m.unpersisted[info.PublicID] = info
	m.mu.Unlock()
}

func (m *Manager) pendingFinal(id string) (Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.unpersisted[id]
	return info, ok
}

// retryUnpersisted tries once more to store the final states the store
// rejected. Entries exist only because the store was failing, so the wait
// ends after persistTimeout or when ctx is done, even if the store ignores
// its context; a retry left running keeps its entries in memory, and later
// calls skip retrying until it returns.
func (m *Manager) retryUnpersisted(ctx context.Context) {
	pending := m.claimUnpersisted()
	if pending == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.persistPending(pending)
	}()
	expired := make(chan struct{})
	bound := m.cfg.AfterFunc(persistTimeout, func() { close(expired) })
	defer bound.Stop()
	select {
	case <-done:
	case <-expired:
		m.cfg.Logger.Warn("terminal session final state retry is still blocked; keeping it in memory", "sessions", len(pending))
	case <-ctx.Done():
	}
}

// claimUnpersisted marks a retry in flight and returns what it should
// store, or nil when there is nothing to do or another retry owns it.
func (m *Manager) claimUnpersisted() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retrying || len(m.unpersisted) == 0 {
		return nil
	}
	m.retrying = true
	pending := make([]Info, 0, len(m.unpersisted))
	for _, info := range m.unpersisted {
		pending = append(pending, info)
	}
	return pending
}

// persistPending stores each pending state, forgetting those that succeed,
// then releases the retry claim.
func (m *Manager) persistPending(pending []Info) {
	storeCtx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	for _, info := range pending {
		if err := m.cfg.Store.UpdateTerminalSession(storeCtx, info); err != nil {
			continue
		}
		m.cfg.Logger.Info("persisted terminal session final state after retry", "sessionId", info.PublicID)
		m.mu.Lock()
		delete(m.unpersisted, info.PublicID)
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.retrying = false
	m.mu.Unlock()
}

func (m *Manager) persist(info Info) bool {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if err := m.cfg.Store.UpdateTerminalSession(ctx, info); err != nil {
		m.cfg.Logger.Error("persist terminal session", "sessionId", info.PublicID, "state", info.State, "error", err)
		return false
	}
	return true
}

// audit records an event. details must never contain terminal input,
// output, or command arguments.
func (m *Manager) audit(eventType, remoteAddr string, details map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	err := m.cfg.Store.AppendAuditEvent(ctx, store.AuditEvent{
		OccurredAt: m.cfg.Now(), Type: eventType, RemoteAddr: remoteAddr, Details: details,
	})
	if err != nil {
		m.cfg.Logger.Error("append audit event", "type", eventType, "error", err)
	}
}

// idBytes gives session IDs 144 bits of randomness (24 base64url characters).
const idBytes = 18

func newID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
