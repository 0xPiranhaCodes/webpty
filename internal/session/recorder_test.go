package session_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

type countingStarter struct {
	inner  *ptytest.Starter
	starts atomic.Int32
}

func (s *countingStarter) Start(cmd pty.Command, size pty.Size) (pty.Process, error) {
	s.starts.Add(1)
	return s.inner.Start(cmd, size)
}

type fakeRecorders struct {
	starter *countingStarter
	mu      sync.Mutex
	began   []session.Info
	starts  []int32
	rec     *fakeRecorder
}

func (f *fakeRecorders) Begin(info session.Info) session.Recorder {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.began = append(f.began, info)
	f.starts = append(f.starts, f.starter.starts.Load())
	f.rec = &fakeRecorder{}
	return f.rec
}

func (f *fakeRecorders) recorder() *fakeRecorder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec
}

type fakeRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *fakeRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *fakeRecorder) Output(data []byte)    { r.add("output:" + string(data)) }
func (r *fakeRecorder) Resize(rows, cols int) { r.add(fmt.Sprintf("resize:%dx%d", rows, cols)) }
func (r *fakeRecorder) Lifecycle(l session.Lifecycle) {
	r.add(lifecycleString("lifecycle", l))
}
func (r *fakeRecorder) End(l session.Lifecycle) { r.add(lifecycleString("end", l)) }

func lifecycleString(prefix string, l session.Lifecycle) string {
	code := "-"
	if l.ExitCode != nil {
		code = fmt.Sprint(*l.ExitCode)
	}
	return fmt.Sprintf("%s:%s:%s:%s:%s", prefix, l.State, code, l.Signal, l.Reason)
}

func (r *fakeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *fakeRecorder) waitFor(t *testing.T, want string) []string {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		events := r.snapshot()
		for _, e := range events {
			if e == want {
				return events
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %v, want %q", events, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func newRecordedHarness(t *testing.T) (*harness, *fakeRecorders) {
	t.Helper()
	var recorders *fakeRecorders
	h := newHarness(t, func(c *session.Config) {
		starter := &countingStarter{inner: c.Starter.(*ptytest.Starter)}
		c.Starter = starter
		recorders = &fakeRecorders{starter: starter}
		c.Recorders = recorders
	})
	return h, recorders
}

func TestRecorderBeginsBeforeProcessAndSeesEventsInOrder(t *testing.T) {
	h, recorders := newRecordedHarness(t)
	info, p := h.create(session.CreateRequest{Rows: 30, Cols: 100})
	if len(recorders.began) != 1 || recorders.starts[0] != 0 {
		t.Fatalf("Begin calls %d with %v processes already started", len(recorders.began), recorders.starts)
	}
	if began := recorders.began[0]; began.PublicID != info.PublicID || began.State != store.TerminalStarting ||
		began.Rows != 30 || began.Cols != 100 {
		t.Fatalf("Begin info = %+v", began)
	}
	rec := recorders.recorder()
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})

	p.Emit("hello")
	nextOutput(t, sub)
	if err := h.m.Write(context.Background(), info.PublicID, []byte("secret-input")); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Resize(info.PublicID, 40, 120); err != nil {
		t.Fatal(err)
	}
	if event := next(t, sub); event.Kind != session.EventResize {
		t.Fatalf("event = %+v, want resize", event)
	}
	p.Emit("world")
	nextOutput(t, sub)
	if _, err := h.m.Terminate(context.Background(), info.PublicID, "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	events := rec.waitFor(t, "end:terminated:-:SIGTERM:admin")
	want := []string{"lifecycle:running:-::", "output:hello", "resize:40x120", "output:world", "end:terminated:-:SIGTERM:admin"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("recorded %v\nwant     %v", events, want)
	}
	if strings.Contains(fmt.Sprint(events), "secret-input") || strings.Contains(fmt.Sprint(events), "192.0.2.9") {
		t.Fatalf("input or address recorded: %v", events)
	}
}

func TestRecorderSeesExitCodeAndNothingAfterEnd(t *testing.T) {
	h, recorders := newRecordedHarness(t)
	info, p := h.create(session.CreateRequest{})
	rec := recorders.recorder()
	p.Exit(pty.ExitStatus{Code: 3})
	h.waitState(info.PublicID, store.TerminalExited)
	events := rec.snapshot()
	if events[len(events)-1] != "end:exited:3::" {
		t.Fatalf("events = %v", events)
	}
}

func TestRecorderEndsWhenStartFails(t *testing.T) {
	h, recorders := newRecordedHarness(t)
	h.starter.FailNext(ptytest.ErrStart)
	if _, err := h.m.Create(context.Background(), session.CreateRequest{}); err == nil {
		t.Fatal("create succeeded")
	}
	rec := recorders.recorder()
	if events := rec.snapshot(); fmt.Sprint(events) != "[end:failed:-::start_failed]" {
		t.Fatalf("events = %v", events)
	}
}

func TestSessionsCanOptOutOfRecording(t *testing.T) {
	h, recorders := newRecordedHarness(t)
	h.create(session.CreateRequest{NoRecording: true})
	if len(recorders.began) != 0 {
		t.Fatalf("opted-out session recorded: %v", recorders.began)
	}
}
