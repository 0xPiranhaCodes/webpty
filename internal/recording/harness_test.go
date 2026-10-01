package recording_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const waitTimeout = 5 * time.Second

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

func (ft *fakeTimers) AfterFunc(d time.Duration, f func()) recording.Timer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	timer := &fakeTimer{timers: ft, d: d, f: f}
	ft.timers = append(ft.timers, timer)
	return timer
}

// fire runs every pending timer of duration d and reports how many ran.
func (ft *fakeTimers) fire(d time.Duration) int {
	ft.mu.Lock()
	var due []*fakeTimer
	for _, timer := range ft.timers {
		if !timer.stopped && !timer.fired && timer.d == d {
			timer.fired = true
			due = append(due, timer)
		}
	}
	ft.mu.Unlock()
	for _, timer := range due {
		timer.f()
	}
	return len(due)
}

func (ft *fakeTimers) pending(d time.Duration) int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	n := 0
	for _, timer := range ft.timers {
		if !timer.stopped && !timer.fired && timer.d == d {
			n++
		}
	}
	return n
}

// faultyStore can block or fail chunk appends.
type faultyStore struct {
	*store.Store
	mu      sync.Mutex
	block   chan struct{}
	fail    error
	txBlock chan struct{}
	// afterChunks runs after every RecordingChunks read.
	afterChunks func()
	// ambiguousCommits makes that many transactions commit, then report
	// failure, as when a commit's outcome is lost to a timeout.
	ambiguousCommits atomic.Int32
	blocked          atomic.Int32
	appends          atomic.Int32
	// abandonedTx counts transactions given up while parked.
	abandonedTx atomic.Int32
}

func (f *faultyStore) AppendRecordingChunk(ctx context.Context, id string, c store.RecordingChunk, now time.Time) error {
	f.mu.Lock()
	block, fail := f.block, f.fail
	f.mu.Unlock()
	if block != nil {
		f.blocked.Add(1)
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail != nil {
		return fail
	}
	f.appends.Add(1)
	return f.Store.AppendRecordingChunk(ctx, id, c, now)
}

// blockAppends parks appends until the returned release is called.
func (f *faultyStore) blockAppends() (release func()) {
	ch := make(chan struct{})
	f.mu.Lock()
	f.block = ch
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.block = nil
			f.mu.Unlock()
			close(ch)
		})
	}
}

// WithTx parks transactions while txBlock is set, until it is released or
// ctx is done.
func (f *faultyStore) WithTx(ctx context.Context, fn func(*store.Tx) error) error {
	f.mu.Lock()
	block := f.txBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.abandonedTx.Add(1)
			return ctx.Err()
		}
	}
	err := f.Store.WithTx(ctx, fn)
	if err == nil && f.ambiguousCommits.Add(-1) >= 0 {
		return context.DeadlineExceeded
	}
	return err
}

func (f *faultyStore) blockTx() (release func()) {
	ch := make(chan struct{})
	f.mu.Lock()
	f.txBlock = ch
	f.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			f.mu.Lock()
			f.txBlock = nil
			f.mu.Unlock()
			close(ch)
		})
	}
	return release
}

// RecordingChunks calls afterChunks, if set, once the read has returned.
func (f *faultyStore) RecordingChunks(ctx context.Context, id string, q store.ChunkQuery) (store.RecordingChunkPage, error) {
	page, err := f.Store.RecordingChunks(ctx, id, q)
	f.mu.Lock()
	after := f.afterChunks
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return page, err
}

func (f *faultyStore) failAppends(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

const (
	flushInterval   = 1500 * time.Millisecond
	shutdownTimeout = 2500 * time.Millisecond
)

type env struct {
	t       *testing.T
	db      *store.Store
	faulty  *faultyStore
	svc     *recording.Service
	clock   *fakeClock
	timers  *fakeTimers
	manager *session.Manager
	starter *ptytest.Starter
	hub     *collab.Hub
}

func newEnv(t *testing.T, configure func(*recording.Config)) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, db: db, faulty: &faultyStore{Store: db}, clock: &fakeClock{now: time.Unix(1_700_000_000, 0)},
		timers: &fakeTimers{}, starter: ptytest.NewStarter(),
	}
	cfg := recording.Config{
		Store:             e.faulty,
		QueueBytes:        64 << 10,
		ChunkBytes:        16 << 10,
		ChunkEvents:       4,
		FlushInterval:     flushInterval,
		Retention:         time.Hour,
		MaxRecordingBytes: 8 << 20,
		ShutdownTimeout:   shutdownTimeout,
		RetentionBatch:    2,
		Now:               e.clock.Now,
		AfterFunc:         e.timers.AfterFunc,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if configure != nil {
		configure(&cfg)
	}
	svc, err := recording.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.svc = svc
	e.hub = collab.NewHub(collab.Config{Observer: svc})
	m, err := session.NewManager(context.Background(), session.Config{
		Store: db, Starter: e.starter, DefaultCommand: pty.Command{Path: "/bin/sh"},
		KillGrace: 50 * time.Millisecond, Recorders: svc, OnEnd: e.hub.EndTerminal, Now: e.clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.manager = m
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		_ = m.Close(ctx)
		e.hub.Close()
		go func() {
			for ctx.Err() == nil {
				e.timers.fire(shutdownTimeout)
				time.Sleep(5 * time.Millisecond)
			}
		}()
		_ = svc.Close(ctx)
		_ = db.Close()
	})
	return e
}

func (e *env) create(req session.CreateRequest) (session.Info, *ptytest.Process) {
	e.t.Helper()
	info, err := e.manager.Create(context.Background(), req)
	if err != nil {
		e.t.Fatalf("Create: %v", err)
	}
	p := e.starter.Next(waitTimeout)
	if p == nil {
		e.t.Fatal("no process started")
	}
	return info, p
}

func (e *env) terminate(id string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if _, err := e.manager.Terminate(ctx, id, ""); err != nil {
		e.t.Fatal(err)
	}
}

// recordingOf returns the only recording of a terminal.
func (e *env) recordingOf(terminalID string) store.Recording {
	e.t.Helper()
	recs, err := e.db.Recordings(context.Background(), store.RecordingFilter{TerminalID: terminalID, Limit: 10})
	if err != nil {
		e.t.Fatal(err)
	}
	if len(recs) != 1 {
		e.t.Fatalf("terminal %s has %d recordings", terminalID, len(recs))
	}
	return recs[0]
}

func (e *env) waitRecording(terminalID string, cond func(store.Recording) bool) store.Recording {
	e.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		recs, err := e.db.Recordings(context.Background(), store.RecordingFilter{TerminalID: terminalID, Limit: 10})
		if err != nil || len(recs) > 1 {
			e.t.Fatalf("recordings of %s = %+v, %v", terminalID, recs, err)
		}
		var rec store.Recording
		if len(recs) == 1 {
			rec = recs[0]
			if cond(rec) {
				return rec
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("recording never reached the wanted state: %+v", rec)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *env) waitStatus(terminalID string, status store.RecordingStatus) store.Recording {
	e.t.Helper()
	return e.waitRecording(terminalID, func(r store.Recording) bool { return r.Status == status })
}

func (e *env) auditEvents(eventType string) []store.AuditEvent {
	e.t.Helper()
	events, err := e.db.AuditEvents(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.AuditEvent
	for _, event := range events {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

func subscribe(t *testing.T, m *session.Manager, id string) *session.Subscription {
	t.Helper()
	sub, err := m.Subscribe(context.Background(), id, session.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	return sub
}

// nextOutput returns the next output, skipping resize notifications.
func nextOutput(t *testing.T, sub *session.Subscription) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	event, err := sub.Next(ctx)
	for err == nil && event.Kind == session.EventResize {
		event, err = sub.Next(ctx)
	}
	if err != nil || event.Kind != session.EventOutput {
		t.Fatalf("Next = %+v, %v", event, err)
	}
	return string(event.Data)
}

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

func (f *faultyStore) setAfterChunks(fn func()) {
	f.mu.Lock()
	f.afterChunks = fn
	f.mu.Unlock()
}
