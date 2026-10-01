package session_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const (
	killGrace   = 7 * time.Second
	idleTimeout = time.Hour
	waitTimeout = 5 * time.Second
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeTimer struct {
	timers  *fakeTimers
	d       time.Duration
	f       func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	t.timers.mu.Lock()
	defer t.timers.mu.Unlock()
	active := !t.stopped && !t.fired
	t.stopped = true
	return active
}

type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (ft *fakeTimers) AfterFunc(d time.Duration, f func()) session.Timer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	timer := &fakeTimer{timers: ft, d: d, f: f}
	ft.timers = append(ft.timers, timer)
	return timer
}

// pending returns active timers matching match.
func (ft *fakeTimers) pending(match func(time.Duration) bool) []*fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []*fakeTimer
	for _, timer := range ft.timers {
		if !timer.stopped && !timer.fired && match(timer.d) {
			out = append(out, timer)
		}
	}
	return out
}

// fire runs every active timer matching match and reports how many ran.
func (ft *fakeTimers) fire(match func(time.Duration) bool) int {
	timers := ft.pending(match)
	for _, timer := range timers {
		ft.mu.Lock()
		timer.fired = true
		ft.mu.Unlock()
		timer.f()
	}
	return len(timers)
}

func isKillGrace(d time.Duration) bool { return d == killGrace }
func isIdle(d time.Duration) bool      { return d != killGrace }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t       *testing.T
	m       *session.Manager
	store   *store.Store
	starter *ptytest.Starter
	clock   *fakeClock
	timers  *fakeTimers
	logs    *syncBuffer
}

func newHarness(t *testing.T, configure func(*session.Config)) *harness {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:       t,
		store:   s,
		starter: ptytest.NewStarter(),
		clock:   &fakeClock{now: time.Unix(1_700_000_000, 0)},
		timers:  &fakeTimers{},
		logs:    &syncBuffer{},
	}
	cfg := session.Config{
		Store:            s,
		Starter:          h.starter,
		DefaultCommand:   pty.Command{Path: "/bin/default-shell", Args: []string{"-l"}},
		MaxSessions:      4,
		MaxViewers:       4,
		IdleTimeout:      idleTimeout,
		KillGrace:        killGrace,
		ReplayBytes:      4096,
		ClientQueueBytes: 8192,
		Logger:           slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:              h.clock.Now,
		AfterFunc:        h.timers.AfterFunc,
	}
	if configure != nil {
		configure(&cfg)
	}
	m, err := session.NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	h.m = m
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		// Fake processes exit on SIGTERM unless told otherwise; force the rest.
		go func() {
			for ctx.Err() == nil {
				h.timers.fire(isKillGrace)
				time.Sleep(10 * time.Millisecond)
			}
		}()
		_ = m.Close(ctx)
		_ = s.Close()
	})
	return h
}

func (h *harness) create(req session.CreateRequest) (session.Info, *ptytest.Process) {
	h.t.Helper()
	info, err := h.m.Create(context.Background(), req)
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	p := h.starter.Next(waitTimeout)
	if p == nil {
		h.t.Fatal("no process started")
	}
	return info, p
}

func (h *harness) subscribe(id string, opts session.SubscribeOptions) *session.Subscription {
	h.t.Helper()
	sub, err := h.m.Subscribe(context.Background(), id, opts)
	if err != nil {
		h.t.Fatalf("Subscribe: %v", err)
	}
	h.t.Cleanup(sub.Close)
	return sub
}

func next(t *testing.T, sub *session.Subscription) session.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	event, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return event
}

func nextOutput(t *testing.T, sub *session.Subscription) session.Event {
	t.Helper()
	event := next(t, sub)
	if event.Kind != session.EventOutput {
		t.Fatalf("event = %+v, want output", event)
	}
	return event
}

func (h *harness) waitState(id string, want store.TerminalState) session.Info {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		info, err := h.m.Get(context.Background(), id)
		if err != nil {
			h.t.Fatalf("Get: %v", err)
		}
		if info.State == want {
			return info
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("state = %q, want %q", info.State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) auditTypes() []string {
	h.t.Helper()
	events, err := h.store.AuditEvents(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var types []string
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func (h *harness) auditEvent(eventType string) store.AuditEvent {
	h.t.Helper()
	events, err := h.store.AuditEvents(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == eventType {
			return event
		}
	}
	h.t.Fatalf("no %s audit event in %v", eventType, h.auditTypes())
	return store.AuditEvent{}
}

func errorIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}
