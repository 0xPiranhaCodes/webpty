package session_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Input accepted while the writer was busy must be re-authorized when the
// PTY is finally ready for it, so a revoked editor cannot deliver it.
func TestQueuedInputIsReauthorizedAtDelivery(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	release := p.BlockWrites()
	var allowed atomic.Bool
	allowed.Store(true)
	allow := allowed.Load

	first := make(chan error, 1)
	go func() { first <- h.m.WriteAuthorized(context.Background(), info.PublicID, []byte("first"), allow) }()
	waitFor(t, "the first write to reach the PTY", func() bool { return p.BlockedWriters() == 1 })
	second := make(chan error, 1)
	go func() { second <- h.m.WriteAuthorized(context.Background(), info.PublicID, []byte("second"), allow) }()
	// The second write is queued behind the first before access is revoked.
	time.Sleep(10 * time.Millisecond)
	allowed.Store(false)
	release()

	if err := <-first; err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := <-second; !errors.Is(err, session.ErrInputRevoked) {
		t.Fatalf("second write = %v, want ErrInputRevoked", err)
	}
	if got := p.Input(); got != "first" {
		t.Fatalf("PTY input = %q, want only the input authorized at delivery", got)
	}
}

func TestResizeIsBroadcastToSubscribers(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	if err := h.m.Resize(info.PublicID, 30, 100); err != nil {
		t.Fatal(err)
	}
	event := next(t, sub)
	if event.Kind != session.EventResize || event.Rows != 30 || event.Cols != 100 {
		t.Fatalf("event = %+v, want resize 30x100", event)
	}
	// Repeating the current size changes nothing and announces nothing.
	if err := h.m.Resize(info.PublicID, 30, 100); err != nil {
		t.Fatal(err)
	}
	p.Emit("after")
	if event := nextOutput(t, sub); string(event.Data) != "after" {
		t.Fatalf("event = %+v", event)
	}
}

// gatedStarter holds Start until released, then makes the process ignore
// SIGTERM so only a SIGKILL ends it.
type gatedStarter struct {
	inner   *ptytest.Starter
	entered chan struct{}
	gate    chan struct{}
	mu      sync.Mutex
	started *ptytest.Process
}

func (g *gatedStarter) Start(cmd pty.Command, size pty.Size) (pty.Process, error) {
	close(g.entered)
	<-g.gate
	proc, err := g.inner.Start(cmd, size)
	if err != nil {
		return nil, err
	}
	p := proc.(*ptytest.Process)
	p.IgnoreTerm()
	g.mu.Lock()
	g.started = p
	g.mu.Unlock()
	return p, nil
}

func TestCloseForceKillsASessionThatFinishesStartingLate(t *testing.T) {
	starter := &gatedStarter{inner: ptytest.NewStarter(), entered: make(chan struct{}), gate: make(chan struct{})}
	h := newHarness(t, func(c *session.Config) {
		c.Starter = starter
		c.ForceReapTimeout = 2 * time.Second
	})
	created := make(chan error, 1)
	go func() {
		_, err := h.m.Create(context.Background(), session.CreateRequest{})
		created <- err
	}()
	<-starter.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- h.m.Close(ctx) }()
	waitFor(t, "Close to pass its deadline", func() bool {
		return h.logs.String() != "" && contains(h.logs.String(), "killing remaining sessions")
	})
	close(starter.gate)

	if err := <-closed; err != nil {
		t.Fatalf("Close = %v; a session that finished starting after the deadline was not killed", err)
	}
	<-created
	starter.mu.Lock()
	p := starter.started
	starter.mu.Unlock()
	if !p.Exited() || !hasSignal(p.Signals(), syscall.SIGKILL) {
		t.Fatalf("late session signals = %v, want SIGKILL", p.Signals())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasSignal(signals []syscall.Signal, want syscall.Signal) bool {
	for _, s := range signals {
		if s == want {
			return true
		}
	}
	return false
}

func TestZeroValueManagerIsSafe(t *testing.T) {
	var m session.Manager
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close on a zero Manager = %v", err)
	}
	if _, err := m.Create(context.Background(), session.CreateRequest{}); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("Create on a zero Manager = %v, want ErrClosed", err)
	}
}

// flakyRepository fails final-state updates while failing is set.
type flakyRepository struct {
	*store.Store
	failing atomic.Bool
}

func (r *flakyRepository) UpdateTerminalSession(ctx context.Context, s store.TerminalSession) error {
	if r.failing.Load() && s.State != store.TerminalRunning {
		return errors.New("disk full")
	}
	return r.Store.UpdateTerminalSession(ctx, s)
}

// wedgedRepository fails final-state writes while failing is set, and
// blocks writes of wedged's session, ignoring their context, until release
// is closed: a SQLite writer that never comes back.
type wedgedRepository struct {
	*store.Store
	failing  atomic.Bool
	wedged   atomic.Value // session public ID
	entered  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (r *wedgedRepository) UpdateTerminalSession(ctx context.Context, s store.TerminalSession) error {
	if id, _ := r.wedged.Load().(string); id == s.PublicID {
		r.entered <- struct{}{}
		<-r.release
		defer func() { r.returned <- struct{}{} }()
		return errors.New("storage unavailable")
	}
	if r.failing.Load() && s.State != store.TerminalRunning {
		return errors.New("disk full")
	}
	return r.Store.UpdateTerminalSession(ctx, s)
}

func isPersistBound(d time.Duration) bool { return d == session.PersistTimeout }

func TestCloseIsBoundedWhenRetryingUnpersistedStateBlocks(t *testing.T) {
	var repo *wedgedRepository
	h := newHarness(t, func(c *session.Config) {
		repo = &wedgedRepository{Store: c.Store.(*store.Store), entered: make(chan struct{}, 8),
			release: make(chan struct{}), returned: make(chan struct{}, 8)}
		c.Store = repo
	})
	t.Cleanup(func() {
		close(repo.release)
		select {
		case <-repo.returned:
		case <-time.After(waitTimeout):
		}
	})

	repo.failing.Store(true)
	exited, p := h.create(session.CreateRequest{})
	p.Exit(pty.ExitStatus{Code: 3})
	h.waitState(exited.PublicID, store.TerminalExited)
	repo.failing.Store(false)
	repo.wedged.Store(exited.PublicID)

	live, liveProc := h.create(session.CreateRequest{})
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		closed <- h.m.Close(ctx)
	}()

	select {
	case <-repo.entered:
	case <-time.After(waitTimeout):
		t.Fatal("Close never retried the unpersisted final state")
	}
	waitFor(t, "Close to arm its retry bound", func() bool { return len(h.timers.pending(isPersistBound)) > 0 })
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v before its retry bound passed", err)
	default:
	}
	h.timers.fire(isPersistBound)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Close stayed blocked behind a wedged store after its retry bound passed")
	}

	if !liveProc.Closed() {
		t.Fatal("the live session's PTY was not closed")
	}
	stored, err := h.store.TerminalSession(context.Background(), live.PublicID)
	if err != nil || stored.State == store.TerminalRunning {
		t.Fatalf("live session stored as %+v, %v; want its final state persisted", stored, err)
	}

	// The retry is still wedged; the final state stays readable from memory.
	got, err := h.m.Get(context.Background(), exited.PublicID)
	if err != nil || got.State != store.TerminalExited || got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("Get after Close = %+v, %v; want the pending exited state", got, err)
	}
	list, err := h.m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range list {
		if info.PublicID == exited.PublicID && info.State != store.TerminalExited {
			t.Fatalf("List after Close shows %s as %s, want exited", info.PublicID, info.State)
		}
	}
	if err := h.m.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestFinalStateIsVisibleAndRetriedWhenPersistingFails(t *testing.T) {
	var repo *flakyRepository
	h := newHarness(t, func(c *session.Config) {
		repo = &flakyRepository{Store: c.Store.(*store.Store)}
		c.Store = repo
	})
	repo.failing.Store(true)
	info, p := h.create(session.CreateRequest{})
	p.Exit(pty.ExitStatus{Code: 3})

	got := h.waitState(info.PublicID, store.TerminalExited)
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("Get = %+v", got)
	}
	list, err := h.m.List(context.Background())
	if err != nil || len(list) != 1 || list[0].State != store.TerminalExited {
		t.Fatalf("List = %+v, %v; want the exited session, not a phantom running row", list, err)
	}
	if !contains(h.logs.String(), "persist terminal session") {
		t.Fatal("persistence failure was not logged")
	}

	repo.failing.Store(false)
	if _, err := h.m.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := h.store.TerminalSession(context.Background(), info.PublicID)
	if err != nil || stored.State != store.TerminalExited {
		t.Fatalf("stored = %+v, %v; want the final state persisted once storage recovered", stored, err)
	}
}
