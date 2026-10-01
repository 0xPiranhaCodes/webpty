package session_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func TestCreateStartsProcessAndPersistsRunningSession(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{
		Command:    pty.Command{Path: "/bin/echo", Args: []string{"a b", "$HOME"}},
		Rows:       30,
		Cols:       100,
		RemoteAddr: "192.0.2.9",
	})

	if info.PublicID == "" || info.State != store.TerminalRunning {
		t.Fatalf("info = %+v, want running with an id", info)
	}
	if p.Command.Path != "/bin/echo" || !reflect.DeepEqual(p.Command.Args, []string{"a b", "$HOME"}) {
		t.Errorf("started %+v, want exact executable and argv", p.Command)
	}
	if got := p.Sizes()[0]; got != (pty.Size{Rows: 30, Cols: 100}) {
		t.Errorf("initial size = %+v", got)
	}

	persisted, err := h.store.TerminalSession(context.Background(), info.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.TerminalRunning || persisted.Command != "/bin/echo" ||
		!reflect.DeepEqual(persisted.Args, []string{"a b", "$HOME"}) || persisted.Rows != 30 || persisted.Cols != 100 ||
		!persisted.CreatedAt.Equal(h.clock.Now()) || !persisted.StartedAt.Equal(h.clock.Now()) {
		t.Errorf("persisted = %+v", persisted)
	}

	if got := h.auditTypes(); !reflect.DeepEqual(got, []string{"terminal.session.created", "terminal.session.started"}) {
		t.Errorf("audit = %v", got)
	}
	created := h.auditEvent("terminal.session.created")
	if created.RemoteAddr != "192.0.2.9" || created.Details["sessionId"] != info.PublicID || created.Details["command"] != "/bin/echo" {
		t.Errorf("created audit = %+v", created)
	}
	if strings.Contains(fmt.Sprint(created.Details), "$HOME") {
		t.Errorf("audit details contain arguments: %v", created.Details)
	}
}

func TestSessionIDsAreUnique(t *testing.T) {
	h := newHarness(t, func(c *session.Config) { c.MaxSessions = 64 })
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		info, _ := h.create(session.CreateRequest{})
		if seen[info.PublicID] {
			t.Fatalf("duplicate id %q", info.PublicID)
		}
		seen[info.PublicID] = true
		if len(info.PublicID) < 20 {
			t.Fatalf("id %q is too short to be unguessable", info.PublicID)
		}
	}
}

func TestCreateUsesDefaultCommandAndSize(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	if p.Command.Path != "/bin/default-shell" || !reflect.DeepEqual(p.Command.Args, []string{"-l"}) {
		t.Errorf("command = %+v, want default", p.Command)
	}
	if info.Rows != 24 || info.Cols != 80 {
		t.Errorf("size = %dx%d, want 24x80", info.Rows, info.Cols)
	}
}

func TestCreateRejectsInvalidRequests(t *testing.T) {
	h := newHarness(t, nil)
	for name, req := range map[string]session.CreateRequest{
		"rows too large":      {Rows: session.MaxRows + 1, Cols: 80},
		"cols too large":      {Rows: 24, Cols: session.MaxCols + 1},
		"negative rows":       {Rows: -1, Cols: 80},
		"args without path":   {Command: pty.Command{Args: []string{"x"}}},
		"nul in path":         {Command: pty.Command{Path: "/bin/sh\x00"}},
		"nul in argument":     {Command: pty.Command{Path: "/bin/sh", Args: []string{"a\x00b"}}},
		"too many arguments":  {Command: pty.Command{Path: "/bin/sh", Args: make([]string, session.MaxArgs+1)}},
		"oversized arguments": {Command: pty.Command{Path: "/bin/sh", Args: []string{strings.Repeat("x", session.MaxCommandBytes)}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.m.Create(context.Background(), req)
			errorIs(t, err, session.ErrInvalidArgument)
		})
	}
	if p := h.starter.Next(10 * time.Millisecond); p != nil {
		t.Fatalf("invalid request started %+v", p.Command)
	}
}

func TestCreateStartFailureIsPersistedAndAudited(t *testing.T) {
	h := newHarness(t, func(c *session.Config) { c.MaxSessions = 1 })
	h.starter.FailNext(ptytest.ErrStart)

	_, err := h.m.Create(context.Background(), session.CreateRequest{Command: pty.Command{Path: "/missing"}})
	errorIs(t, err, session.ErrProcessFailed)

	list, err := h.m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != store.TerminalFailed || list[0].Failure == "" || list[0].EndedAt.IsZero() {
		t.Fatalf("list = %+v, want one failed session", list)
	}
	if got := h.auditTypes(); !reflect.DeepEqual(got, []string{"terminal.session.created", "terminal.session.failed"}) {
		t.Errorf("audit = %v", got)
	}
	// A failed start does not consume the only slot.
	h.create(session.CreateRequest{})
}

func TestMaxActiveSessions(t *testing.T) {
	h := newHarness(t, func(c *session.Config) { c.MaxSessions = 2 })
	_, first := h.create(session.CreateRequest{})
	h.create(session.CreateRequest{})

	_, err := h.m.Create(context.Background(), session.CreateRequest{})
	errorIs(t, err, session.ErrSessionLimit)

	first.Exit(pty.ExitStatus{Code: 0})
	deadline := time.Now().Add(waitTimeout)
	for {
		_, err := h.m.Create(context.Background(), session.CreateRequest{})
		if err == nil {
			break
		}
		if !errors.Is(err, session.ErrSessionLimit) || time.Now().After(deadline) {
			t.Fatalf("Create after exit: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOutputFansOutWithMonotonicSequence(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	a := h.subscribe(info.PublicID, session.SubscribeOptions{})
	b := h.subscribe(info.PublicID, session.SubscribeOptions{})

	for _, chunk := range []string{"one", "two", "three"} {
		p.Emit(chunk)
		for _, sub := range []*session.Subscription{a, b} {
			event := nextOutput(t, sub)
			if string(event.Data) != chunk {
				t.Fatalf("data = %q, want %q", event.Data, chunk)
			}
		}
	}
	p.Emit("four")
	if event := nextOutput(t, a); event.Seq != 4 {
		t.Fatalf("seq = %d, want 4", event.Seq)
	}
}

func emitAndDrain(t *testing.T, p *ptytest.Process, sub *session.Subscription, chunks ...string) {
	t.Helper()
	for _, chunk := range chunks {
		p.Emit(chunk)
		nextOutput(t, sub)
	}
}

func TestReconnectReplaysAfterSequence(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	first := h.subscribe(info.PublicID, session.SubscribeOptions{})
	emitAndDrain(t, p, first, "a", "b", "c")
	first.Close()

	all := h.subscribe(info.PublicID, session.SubscribeOptions{})
	if all.LastSeq() != 3 {
		t.Fatalf("LastSeq = %d, want 3", all.LastSeq())
	}
	for want := uint64(1); want <= 3; want++ {
		if event := nextOutput(t, all); event.Seq != want {
			t.Fatalf("replayed seq = %d, want %d", event.Seq, want)
		}
	}

	resumed := h.subscribe(info.PublicID, session.SubscribeOptions{Resume: true, AfterSeq: 1})
	if event := nextOutput(t, resumed); event.Seq != 2 || string(event.Data) != "b" {
		t.Fatalf("resumed = %+v, want seq 2", event)
	}
	if event := nextOutput(t, resumed); event.Seq != 3 {
		t.Fatalf("resumed = %+v, want seq 3", event)
	}
	p.Emit("d")
	if event := nextOutput(t, resumed); event.Seq != 4 || string(event.Data) != "d" {
		t.Fatalf("live after replay = %+v", event)
	}

	current := h.subscribe(info.PublicID, session.SubscribeOptions{Resume: true, AfterSeq: 4})
	p.Emit("e")
	if event := nextOutput(t, current); event.Seq != 5 {
		t.Fatalf("up-to-date resume got seq %d, want 5", event.Seq)
	}
}

func TestReplayGapIsTyped(t *testing.T) {
	h := newHarness(t, func(c *session.Config) {
		c.ReplayBytes = 2 * (session.EventOverhead + 4)
		c.ClientQueueBytes = 1 << 20
	})
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	emitAndDrain(t, p, sub, "aaaa", "bbbb", "cccc")

	_, err := h.m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{Resume: true, AfterSeq: 0})
	errorIs(t, err, session.ErrReplayGap)
	var gap *session.ReplayGapError
	if !errors.As(err, &gap) || gap.FirstSeq != 2 || gap.AfterSeq != 0 {
		t.Fatalf("gap = %+v, want FirstSeq 2 AfterSeq 0", gap)
	}

	_, err = h.m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{Resume: true, AfterSeq: 99})
	errorIs(t, err, session.ErrReplayGap)

	ok := h.subscribe(info.PublicID, session.SubscribeOptions{Resume: true, AfterSeq: 1})
	if event := nextOutput(t, ok); event.Seq != 2 {
		t.Fatalf("seq = %d, want 2", event.Seq)
	}
	// A fresh subscription replays what remains without a gap error.
	fresh := h.subscribe(info.PublicID, session.SubscribeOptions{})
	if event := nextOutput(t, fresh); event.Seq != 2 {
		t.Fatalf("fresh seq = %d, want 2", event.Seq)
	}
}

func TestMaxViewersPerSession(t *testing.T) {
	h := newHarness(t, func(c *session.Config) { c.MaxViewers = 1 })
	info, _ := h.create(session.CreateRequest{})
	first := h.subscribe(info.PublicID, session.SubscribeOptions{})

	_, err := h.m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{})
	errorIs(t, err, session.ErrViewerLimit)

	first.Close()
	first.Close()
	h.subscribe(info.PublicID, session.SubscribeOptions{})
}

func TestSlowConsumerIsClosedWithoutBlockingOthers(t *testing.T) {
	h := newHarness(t, func(c *session.Config) {
		c.ReplayBytes = 64
		c.ClientQueueBytes = 3 * (session.EventOverhead + 10)
	})
	info, p := h.create(session.CreateRequest{})
	slow := h.subscribe(info.PublicID, session.SubscribeOptions{})
	fast := h.subscribe(info.PublicID, session.SubscribeOptions{})

	for i := 0; i < 10; i++ {
		chunk := fmt.Sprintf("chunk-%04d", i)
		p.Emit(chunk)
		if event := nextOutput(t, fast); string(event.Data) != chunk {
			t.Fatalf("fast got %q, want %q", event.Data, chunk)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	var err error
	for err == nil {
		_, err = slow.Next(ctx)
	}
	errorIs(t, err, session.ErrSlowConsumer)

	// The slot is released and the session keeps running.
	if _, err := h.m.Get(context.Background(), info.PublicID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "slow") {
		t.Errorf("slow consumer disconnect not logged: %s", h.logs.String())
	}
}

func TestWriteAndResizeReachTheProcess(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	h.clock.Advance(time.Minute)

	if err := h.m.Write(context.Background(), info.PublicID, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	if p.Input() != "ls\r" {
		t.Fatalf("input = %q", p.Input())
	}
	if err := h.m.Resize(info.PublicID, 50, 200); err != nil {
		t.Fatal(err)
	}
	if got := p.Sizes(); got[len(got)-1] != (pty.Size{Rows: 50, Cols: 200}) {
		t.Fatalf("sizes = %+v", got)
	}
	got, err := h.m.Get(context.Background(), info.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rows != 50 || got.Cols != 200 || !got.LastActivityAt.Equal(h.clock.Now()) {
		t.Fatalf("info = %+v, want 50x200 and fresh activity", got)
	}

	for _, size := range [][2]int{{0, 80}, {24, 0}, {session.MaxRows + 1, 80}, {24, session.MaxCols + 1}} {
		errorIs(t, h.m.Resize(info.PublicID, size[0], size[1]), session.ErrInvalidArgument)
	}
	errorIs(t, h.m.Write(context.Background(), "missing", []byte("x")), session.ErrNotFound)
	errorIs(t, h.m.Resize("missing", 24, 80), session.ErrNotFound)
}

func TestNaturalExitDeliversOutputThenExitAndReleasesResources(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	if err := h.m.Resize(info.PublicID, 40, 90); err != nil {
		t.Fatal(err)
	}
	if event := next(t, sub); event.Kind != session.EventResize {
		t.Fatalf("event = %+v, want resize", event)
	}

	p.Emit("last words")
	p.Exit(pty.ExitStatus{Code: 2})

	if event := nextOutput(t, sub); string(event.Data) != "last words" {
		t.Fatalf("data = %q", event.Data)
	}
	exit := next(t, sub)
	if exit.Kind != session.EventExit || exit.Exit.State != store.TerminalExited || exit.Exit.Code == nil || *exit.Exit.Code != 2 {
		t.Fatalf("exit event = %+v", exit)
	}
	if _, err := sub.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after exit err = %v, want EOF", err)
	}

	final := h.waitState(info.PublicID, store.TerminalExited)
	if final.ExitCode == nil || *final.ExitCode != 2 || final.EndedAt.IsZero() || final.Rows != 40 || final.Cols != 90 {
		t.Fatalf("final = %+v", final)
	}
	if !p.Closed() {
		t.Error("PTY not closed after exit")
	}
	exited := h.auditEvent("terminal.session.exited")
	if exited.Details["exitCode"] != "2" {
		t.Errorf("exited audit = %+v", exited)
	}

	errorIs(t, h.m.Write(context.Background(), info.PublicID, []byte("x")), session.ErrInvalidState)
	_, err := h.m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{})
	errorIs(t, err, session.ErrInvalidState)
}

func TestOnEndIsCalledOnceAfterTheFinalStateIsDurable(t *testing.T) {
	type ended struct {
		id    string
		state store.TerminalState
	}
	calls := make(chan ended, 4)
	var h *harness
	h = newHarness(t, func(c *session.Config) {
		c.OnEnd = func(id string) {
			stored, err := h.store.TerminalSession(context.Background(), id)
			if err != nil {
				t.Errorf("OnEnd lookup: %v", err)
			}
			calls <- ended{id, stored.State}
		}
	})
	exiting, p := h.create(session.CreateRequest{})
	terminated, _ := h.create(session.CreateRequest{})

	p.Exit(pty.ExitStatus{Code: 0})
	if _, err := h.m.Terminate(context.Background(), terminated.PublicID, ""); err != nil {
		t.Fatal(err)
	}
	got := map[string]store.TerminalState{}
	for i := 0; i < 2; i++ {
		select {
		case call := <-calls:
			got[call.id] = call.state
		case <-time.After(waitTimeout):
			t.Fatal("OnEnd not called")
		}
	}
	if got[exiting.PublicID] != store.TerminalExited || got[terminated.PublicID] != store.TerminalTerminated {
		t.Fatalf("OnEnd calls = %v", got)
	}
	select {
	case extra := <-calls:
		t.Fatalf("extra OnEnd call %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestWaitFailureMarksSessionFailed(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	p.FailWait(errors.New("wait broke"))
	final := h.waitState(info.PublicID, store.TerminalFailed)
	if final.Failure == "" {
		t.Fatalf("final = %+v, want failure reason", final)
	}
	h.auditEvent("terminal.session.failed")
	if !p.Closed() {
		t.Error("PTY not closed after failed wait")
	}
}

func TestTerminateSignalsGroupAndIsIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})

	final, err := h.m.Terminate(context.Background(), info.PublicID, "192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	if final.State != store.TerminalTerminated || final.ExitSignal != "SIGTERM" {
		t.Fatalf("final = %+v, want terminated by SIGTERM", final)
	}
	if got := p.Signals(); len(got) == 0 || got[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want SIGTERM first", got)
	}
	exit := next(t, sub)
	if exit.Kind != session.EventExit || exit.Exit.State != store.TerminalTerminated || exit.Exit.Signal != "SIGTERM" {
		t.Fatalf("exit = %+v", exit)
	}
	terminated := h.auditEvent("terminal.session.terminated")
	if terminated.RemoteAddr != "192.0.2.7" || terminated.Details["reason"] != "admin" {
		t.Errorf("terminated audit = %+v", terminated)
	}

	signals := len(p.Signals())
	again, err := h.m.Terminate(context.Background(), info.PublicID, "192.0.2.7")
	if err != nil || again.State != store.TerminalTerminated {
		t.Fatalf("second Terminate = %+v, %v", again, err)
	}
	if len(p.Signals()) != signals {
		t.Errorf("second Terminate sent more signals: %v", p.Signals())
	}
	_, err = h.m.Terminate(context.Background(), "missing", "")
	errorIs(t, err, session.ErrNotFound)
}

func TestTerminateEscalatesToKillAfterGrace(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	p.IgnoreTerm()

	done := make(chan session.Info, 1)
	go func() {
		final, err := h.m.Terminate(context.Background(), info.PublicID, "")
		if err != nil {
			t.Error(err)
		}
		done <- final
	}()

	deadline := time.Now().Add(waitTimeout)
	for len(h.timers.pending(isKillGrace)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no kill grace timer armed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("Terminate returned before the process exited")
	case <-time.After(20 * time.Millisecond):
	}
	if got := p.Signals(); !reflect.DeepEqual(got, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals before grace = %v", got)
	}

	h.timers.fire(isKillGrace)
	select {
	case final := <-done:
		if final.State != store.TerminalTerminated || final.ExitSignal != "SIGKILL" {
			t.Fatalf("final = %+v, want terminated by SIGKILL", final)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Terminate did not return after SIGKILL")
	}
}

func TestKillTimerIsStoppedWhenProcessExitsDuringGrace(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	p.IgnoreTerm()
	go func() { _, _ = h.m.Terminate(context.Background(), info.PublicID, "") }()
	deadline := time.Now().Add(waitTimeout)
	for len(h.timers.pending(isKillGrace)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no kill grace timer armed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.Exit(pty.ExitStatus{Code: 0})
	h.waitState(info.PublicID, store.TerminalTerminated)
	if pending := h.timers.pending(isKillGrace); len(pending) != 0 {
		t.Fatalf("kill timer still pending after exit")
	}
}

func TestSessionSurvivesClientDisconnect(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	emitAndDrain(t, p, sub, "before")
	sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, session.ErrSubscriptionClosed) {
		t.Fatalf("Next after Close err = %v", err)
	}
	p.Emit("while detached")
	if got := h.waitState(info.PublicID, store.TerminalRunning); got.State != store.TerminalRunning {
		t.Fatal("session stopped after disconnect")
	}
	again := h.subscribe(info.PublicID, session.SubscribeOptions{Resume: true, AfterSeq: 1})
	if event := nextOutput(t, again); string(event.Data) != "while detached" {
		t.Fatalf("replay after reconnect = %q", event.Data)
	}
	if p.Exited() {
		t.Fatal("process ended on disconnect")
	}
}

func TestIdleTimeoutTerminatesDetachedSessions(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})

	// Attached sessions are never idle.
	h.clock.Advance(2 * idleTimeout)
	h.timers.fire(isIdle)
	if p.Exited() {
		t.Fatal("attached session terminated as idle")
	}

	sub.Close()
	h.clock.Advance(idleTimeout / 2)
	h.timers.fire(isIdle)
	if p.Exited() {
		t.Fatal("session terminated before the idle timeout elapsed")
	}
	h.clock.Advance(idleTimeout / 2)
	if h.timers.fire(isIdle) == 0 {
		t.Fatal("idle timer not re-armed")
	}
	final := h.waitState(info.PublicID, store.TerminalTerminated)
	if final.ExitSignal != "SIGTERM" {
		t.Fatalf("final = %+v", final)
	}
	if event := h.auditEvent("terminal.session.terminated"); event.Details["reason"] != "idle" {
		t.Fatalf("terminated audit = %+v", event)
	}
}

func TestDetachedOutputKeepsSessionActive(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	h.clock.Advance(idleTimeout - time.Minute)
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	emitAndDrain(t, p, sub, "busy")
	sub.Close()
	h.clock.Advance(idleTimeout - time.Second)
	h.timers.fire(isIdle)
	if p.Exited() {
		t.Fatal("recently active session terminated as idle")
	}
	if got := h.waitState(info.PublicID, store.TerminalRunning); got.State != store.TerminalRunning {
		t.Fatal("not running")
	}
}

func TestCloseTerminatesEverySessionAndRejectsNewOnes(t *testing.T) {
	h := newHarness(t, nil)
	a, pa := h.create(session.CreateRequest{})
	b, pb := h.create(session.CreateRequest{})
	pb.IgnoreTerm()

	closed := make(chan error, 1)
	go func() { closed <- h.m.Close(context.Background()) }()
	deadline := time.Now().Add(waitTimeout)
	for len(h.timers.pending(isKillGrace)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no kill timer during Close")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.timers.fire(isKillGrace)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Close did not return")
	}
	if !pa.Exited() || !pb.Exited() || !pa.Closed() || !pb.Closed() {
		t.Fatal("Close left a process or PTY behind")
	}
	for _, id := range []string{a.PublicID, b.PublicID} {
		if info := h.waitState(id, store.TerminalTerminated); info.EndedAt.IsZero() {
			t.Errorf("%s has no end time", id)
		}
	}
	if event := h.auditEvent("terminal.session.terminated"); event.Details["reason"] != "shutdown" {
		t.Errorf("terminated audit = %+v", event)
	}
	_, err := h.m.Create(context.Background(), session.CreateRequest{})
	errorIs(t, err, session.ErrClosed)
	if err := h.m.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNewManagerFailsSessionsInterruptedByRestart(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.store.CreateTerminalSession(context.Background(), store.TerminalSession{
		PublicID: "stale", State: store.TerminalRunning, Command: "/bin/sh", Rows: 24, Cols: 80,
		CreatedAt: time.Unix(1, 0), LastActivityAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.NewManager(context.Background(), session.Config{Store: h.store, Starter: h.starter}); err != nil {
		t.Fatal(err)
	}
	info, err := h.m.Get(context.Background(), "stale")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != store.TerminalFailed {
		t.Fatalf("stale state = %q, want failed", info.State)
	}
}

func TestNewManagerValidatesLimits(t *testing.T) {
	h := newHarness(t, nil)
	_, err := session.NewManager(context.Background(), session.Config{
		Store: h.store, Starter: h.starter, ReplayBytes: 2048, ClientQueueBytes: 1024,
	})
	if err == nil {
		t.Fatal("client queue smaller than the replay buffer accepted")
	}
}

func TestTerminalDataNeverReachesLogsOrAudit(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "argument-secret"}}})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	if err := h.m.Write(context.Background(), info.PublicID, []byte("input-secret")); err != nil {
		t.Fatal(err)
	}
	emitAndDrain(t, p, sub, "output-secret")
	if _, err := h.m.Terminate(context.Background(), info.PublicID, ""); err != nil {
		t.Fatal(err)
	}

	events, err := h.store.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	haystack := h.logs.String() + fmt.Sprint(events)
	for _, secret := range []string{"input-secret", "output-secret", "argument-secret"} {
		if strings.Contains(haystack, secret) {
			t.Errorf("%q leaked into logs or audit: %s", secret, haystack)
		}
	}
	for _, msg := range []string{"terminal session started", "terminal session terminated"} {
		if !strings.Contains(h.logs.String(), msg) {
			t.Errorf("log %q missing: %s", msg, h.logs.String())
		}
	}
}

func TestWriteReturnsWhenContextEndsWhilePTYBlocked(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	release := p.BlockWrites()
	defer release()

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		start := time.Now()
		err := h.m.Write(ctx, info.PublicID, []byte("stuck"))
		cancel()
		errorIs(t, err, context.DeadlineExceeded)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Write took %v with an expired context", elapsed)
		}
	}
	if n := p.BlockedWriters(); n != 1 {
		t.Fatalf("blocked PTY writes = %d, want exactly one session-owned writer", n)
	}
	emitAndDrain(t, p, sub, "output still flows")
	if err := h.m.Resize(info.PublicID, 30, 90); err != nil {
		t.Fatalf("Resize while input blocked: %v", err)
	}

	if _, err := h.m.Terminate(context.Background(), info.PublicID, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitTimeout)
	for p.BlockedWriters() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("blocked PTY write not released by termination")
		}
		time.Sleep(5 * time.Millisecond)
	}
	errorIs(t, h.m.Write(context.Background(), info.PublicID, []byte("x")), session.ErrInvalidState)
}

func TestBlockedInputDoesNotPreventIdleTermination(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	release := p.BlockWrites()
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_ = h.m.Write(ctx, info.PublicID, []byte("stuck"))
	cancel()
	sub.Close()

	h.clock.Advance(idleTimeout)
	if h.timers.fire(isIdle) == 0 {
		t.Fatal("idle timer not armed after the last viewer left")
	}
	h.waitState(info.PublicID, store.TerminalTerminated)
	if n := p.BlockedWriters(); n != 0 {
		t.Fatalf("blocked writes = %d after termination", n)
	}
}

func TestSlowInputDoesNotBlockOutput(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	sub := h.subscribe(info.PublicID, session.SubscribeOptions{})
	release := p.BlockWrites()
	defer release()

	go func() { _ = h.m.Write(context.Background(), info.PublicID, []byte("stuck")) }()
	time.Sleep(10 * time.Millisecond)
	emitAndDrain(t, p, sub, "still flowing")
	if _, err := h.m.Get(context.Background(), info.PublicID); err != nil {
		t.Fatal(err)
	}
	release()
}

func TestConcurrentLifecycleOperations(t *testing.T) {
	h := newHarness(t, func(c *session.Config) {
		c.MaxSessions = 64
		c.MaxViewers = 64
	})
	const sessions = 8
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		info, p := h.create(session.CreateRequest{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			var inner sync.WaitGroup
			for j := 0; j < 4; j++ {
				inner.Add(1)
				go func() {
					defer inner.Done()
					sub, err := h.m.Subscribe(context.Background(), info.PublicID, session.SubscribeOptions{})
					if err != nil {
						return
					}
					defer sub.Close()
					ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
					defer cancel()
					for {
						event, err := sub.Next(ctx)
						if err != nil || event.Kind == session.EventExit {
							return
						}
					}
				}()
			}
			for j := 0; j < 20; j++ {
				p.Emit(fmt.Sprintf("line %d\n", j))
				_ = h.m.Write(context.Background(), info.PublicID, []byte("k"))
				_ = h.m.Resize(info.PublicID, 24+j, 80)
				_, _ = h.m.List(context.Background())
			}
			var terminators sync.WaitGroup
			for j := 0; j < 3; j++ {
				terminators.Add(1)
				go func() {
					defer terminators.Done()
					if _, err := h.m.Terminate(context.Background(), info.PublicID, ""); err != nil {
						t.Error(err)
					}
				}()
			}
			terminators.Wait()
			inner.Wait()
		}()
	}
	wg.Wait()
	list, err := h.m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range list {
		if info.State != store.TerminalTerminated {
			t.Errorf("%s state = %q", info.PublicID, info.State)
		}
	}
}

func TestCloseForcesKillWhenContextExpiresBeforeKillGrace(t *testing.T) {
	h := newHarness(t, nil)
	info, p := h.create(session.CreateRequest{})
	p.IgnoreTerm()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.m.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want nil once the forced kill reaped every session", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	if !p.Exited() || !p.Closed() {
		t.Fatalf("process exited=%v closed=%v after Close returned", p.Exited(), p.Closed())
	}
	if signals := p.Signals(); len(signals) < 2 || signals[len(signals)-1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want SIGTERM then SIGKILL", signals)
	}
	stored, err := h.store.TerminalSession(context.Background(), info.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != store.TerminalTerminated || stored.ExitSignal != "SIGKILL" {
		t.Fatalf("stored = %+v, want terminated by SIGKILL", stored)
	}
	if h.timers.fire(isKillGrace) != 0 {
		t.Fatal("kill-grace timer still pending after forced kill")
	}
}
