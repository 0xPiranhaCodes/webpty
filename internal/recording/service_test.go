package recording_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func allEvents(t *testing.T, svc *recording.Service, id string) []recording.Event {
	t.Helper()
	page, err := svc.Events(context.Background(), id, recording.Query{Limit: 1000}, "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if page.Next != nil {
		t.Fatalf("unexpected next cursor %+v", page.Next)
	}
	return page.Events
}

func describe(events []recording.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		var s string
		switch e.Kind {
		case recording.KindOutput:
			s = "output:" + string(e.Output)
		case recording.KindResize:
			s = fmt.Sprintf("resize:%dx%d", e.Rows, e.Cols)
		case recording.KindLifecycle:
			s = "lifecycle:" + e.Lifecycle.State + ":" + e.Lifecycle.Reason
		case recording.KindPresence:
			s = "presence:" + e.Presence.Event + ":" + e.Presence.Role
		}
		out = append(out, fmt.Sprintf("%d@%d %s", e.Seq, e.OffsetMS, s))
	}
	return out
}

func rawChunks(t *testing.T, db *store.Store, id string) string {
	t.Helper()
	rows, err := db.DB().Query(`SELECT c.data FROM recording_chunks c JOIN recordings r ON r.id = c.recording_id
		WHERE r.public_id = ? ORDER BY c.chunk_index`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all strings.Builder
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(plain)
	}
	return all.String()
}

func TestRecordsOutputResizeLifecycleAndPresenceButNeverInput(t *testing.T) {
	e := newEnv(t, nil)
	info, p := e.create(session.CreateRequest{Rows: 30, Cols: 100})
	sub := subscribe(t, e.manager, info.PublicID)

	e.clock.Advance(10 * time.Millisecond)
	p.Emit("hello")
	nextOutput(t, sub)
	if err := e.manager.Write(context.Background(), info.PublicID, []byte("hunter2-secret")); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(15 * time.Millisecond)
	if err := e.manager.Resize(info.PublicID, 40, 120); err != nil {
		t.Fatal(err)
	}
	viewer, err := e.hub.Join(collab.JoinRequest{TerminalID: info.PublicID, Role: collab.RoleViewer, GrantID: "grant-secret", SessionID: 99})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(5 * time.Millisecond)
	viewer.Leave()
	p.Emit("bye")
	nextOutput(t, sub)
	e.terminate(info.PublicID)

	rec := e.waitStatus(info.PublicID, store.RecordingComplete)
	if rec.Rows != 30 || rec.Cols != 100 || rec.EventCount != 7 || rec.ChunkCount != 2 || rec.DurationMS != 30 ||
		rec.FormatVersion != 1 || rec.Codec != "gzip" || !rec.RetainUntil.Equal(rec.EndedAt.Add(time.Hour)) {
		t.Fatalf("recording = %+v", rec)
	}
	want := []string{
		"1@0 lifecycle:running:", "2@10 output:hello", "3@25 resize:40x120", "4@25 presence:joined:viewer",
		"5@30 presence:left:viewer", "6@30 output:bye", "7@30 lifecycle:terminated:admin",
	}
	if got := describe(allEvents(t, e.svc, rec.PublicID)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %v\nwant   %v", got, want)
	}

	raw := rawChunks(t, e.db, rec.PublicID)
	for _, secret := range []string{"hunter2", "hello", "grant-secret", "/bin/sh", "192.0.2"} {
		if strings.Contains(raw, secret) {
			t.Errorf("stored chunks contain %q:\n%s", secret, raw)
		}
	}
	if !strings.Contains(raw, `"aGVsbG8="`) {
		t.Errorf("output is not stored as base64:\n%s", raw)
	}
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		var record struct {
			V int `json:"v"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.V != 1 {
			t.Errorf("line %q is not a version 1 record", line)
		}
	}

	if started := e.auditEvents("recording.started"); len(started) != 1 || started[0].Details["recordingId"] != rec.PublicID {
		t.Fatalf("started audits = %+v", started)
	}
	completed := e.auditEvents("recording.completed")
	if len(completed) != 1 || completed[0].Details["events"] != "7" || completed[0].Details["chunks"] != "2" {
		t.Fatalf("completed audits = %+v", completed)
	}
}

func TestPartialChunksFlushOnTheFlushTimer(t *testing.T) {
	e := newEnv(t, nil)
	info, p := e.create(session.CreateRequest{})
	sub := subscribe(t, e.manager, info.PublicID)
	p.Emit("x")
	nextOutput(t, sub)
	waitFor(t, "flush timer", func() bool { return e.timers.pending(flushInterval) == 1 })
	if rec := e.recordingOf(info.PublicID); rec.ChunkCount != 0 {
		t.Fatalf("flushed before the interval: %+v", rec)
	}
	e.timers.fire(flushInterval)
	rec := e.waitRecording(info.PublicID, func(r store.Recording) bool { return r.ChunkCount == 1 })
	if rec.Status != store.RecordingActive || rec.EventCount != 2 {
		t.Fatalf("after flush = %+v", rec)
	}
}

func TestRecordingDisabledOrOptedOut(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.Disabled = true })
	info, _ := e.create(session.CreateRequest{})
	recs, _ := e.db.Recordings(context.Background(), store.RecordingFilter{TerminalID: info.PublicID, Limit: 1})
	if len(recs) != 0 {
		t.Fatalf("disabled service recorded %+v", recs)
	}

	e = newEnv(t, nil)
	info, _ = e.create(session.CreateRequest{NoRecording: true})
	recs, _ = e.db.Recordings(context.Background(), store.RecordingFilter{TerminalID: info.PublicID, Limit: 1})
	if len(recs) != 0 {
		t.Fatalf("opted-out session recorded %+v", recs)
	}
}

func TestBlockedStoreNeverBlocksTheLiveSessionAndOverflowsSafely(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.QueueBytes = 16 << 10; c.ChunkEvents = 1 })
	release := e.faulty.blockAppends()
	defer release()
	info, p := e.create(session.CreateRequest{})
	statuses, cancel := e.svc.Watch(info.PublicID)
	defer cancel()
	sub := subscribe(t, e.manager, info.PublicID)
	waitFor(t, "blocked append", func() bool { return e.faulty.blocked.Load() > 0 })

	chunk := strings.Repeat("z", 1024)
	for i := 0; i < 40; i++ {
		p.Emit(chunk)
		if got := nextOutput(t, sub); got != chunk {
			t.Fatalf("output %d = %q", i, got)
		}
	}
	if err := e.manager.Write(context.Background(), info.PublicID, []byte("still-alive")); err != nil {
		t.Fatalf("input after overflow: %v", err)
	}
	joined, err := e.hub.Join(collab.JoinRequest{TerminalID: info.PublicID, Role: collab.RoleOwner})
	if err != nil {
		t.Fatalf("presence blocked: %v", err)
	}
	joined.Leave()
	release()

	select {
	case st := <-statuses:
		if st.Code != recording.CodeQueueOverflow || st.State != store.RecordingIncomplete || st.RecordingID == "" {
			t.Fatalf("status = %+v", st)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no recording status")
	}
	rec := e.waitStatus(info.PublicID, store.RecordingIncomplete)
	if rec.FailureCode != recording.CodeQueueOverflow {
		t.Fatalf("recording = %+v", rec)
	}
	events := allEvents(t, e.svc, rec.PublicID)
	if int64(len(events)) != rec.EventCount || rec.EventCount == 0 {
		t.Fatalf("flushed %d events, recording claims %d", len(events), rec.EventCount)
	}
	if audits := e.auditEvents("recording.incomplete"); len(audits) != 1 || audits[0].Details["code"] != "queue_overflow" {
		t.Fatalf("incomplete audits = %+v", audits)
	}

	p.Emit("after")
	if got := nextOutput(t, sub); got != "after" {
		t.Fatalf("live output after failure = %q", got)
	}
	if again, cancel := e.svc.Watch(info.PublicID); true {
		defer cancel()
		select {
		case st := <-again:
			if st.Code != recording.CodeQueueOverflow {
				t.Fatalf("late watcher status = %+v", st)
			}
		default:
			t.Fatal("late watcher not told about the failure")
		}
	}
}

func TestStorageFailureMarksIncompleteAndLiveSessionContinues(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.ChunkEvents = 1 })
	e.faulty.failAppends(errors.New("disk I/O error: /var/secret/path"))
	info, p := e.create(session.CreateRequest{})
	statuses, cancel := e.svc.Watch(info.PublicID)
	defer cancel()
	sub := subscribe(t, e.manager, info.PublicID)
	p.Emit("a")
	nextOutput(t, sub)

	rec := e.waitStatus(info.PublicID, store.RecordingIncomplete)
	if rec.FailureCode != recording.CodeStorage {
		t.Fatalf("recording = %+v", rec)
	}
	st := <-statuses
	if st.Code != recording.CodeStorage || strings.Contains(fmt.Sprint(st), "secret") {
		t.Fatalf("status = %+v", st)
	}
	for _, audit := range e.auditEvents("recording.incomplete") {
		if strings.Contains(fmt.Sprint(audit.Details), "secret") {
			t.Fatalf("audit leaked the error: %+v", audit)
		}
	}
	p.Emit("b")
	if got := nextOutput(t, sub); got != "b" {
		t.Fatalf("output after storage failure = %q", got)
	}
}

func TestEncodingFailureMarksIncomplete(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.ChunkEvents = 1 })
	recording.FailEncoding(e.svc, recording.KindOutput)
	info, p := e.create(session.CreateRequest{})
	sub := subscribe(t, e.manager, info.PublicID)
	p.Emit("a")
	nextOutput(t, sub)
	rec := e.waitStatus(info.PublicID, store.RecordingIncomplete)
	if rec.FailureCode != recording.CodeEncoding || rec.EventCount != 1 {
		t.Fatalf("recording = %+v", rec)
	}
}

func TestRecordingSizeLimit(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.ChunkEvents = 1; c.ChunkBytes = 4 << 10; c.MaxRecordingBytes = 4 << 10 })
	info, p := e.create(session.CreateRequest{})
	sub := subscribe(t, e.manager, info.PublicID)
	for i := 0; i < 8; i++ {
		p.Emit(strings.Repeat("y", 1000))
		nextOutput(t, sub)
	}
	rec := e.waitStatus(info.PublicID, store.RecordingIncomplete)
	if rec.FailureCode != recording.CodeSizeLimit || rec.UncompressedBytes > 4<<10 {
		t.Fatalf("recording = %+v", rec)
	}
}

func TestCloseFlushesEndedRecordings(t *testing.T) {
	e := newEnv(t, nil)
	info, p := e.create(session.CreateRequest{})
	p.Emit("x")
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := e.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec := e.recordingOf(info.PublicID)
	if rec.Status != store.RecordingComplete {
		t.Fatalf("recording after Close = %+v", rec)
	}
	last := allEvents(t, e.svc, rec.PublicID)
	if l := last[len(last)-1]; l.Kind != recording.KindLifecycle || l.Lifecycle.Reason != "shutdown" {
		t.Fatalf("final event = %+v", l)
	}
	if _, err := e.manager.Create(context.Background(), session.CreateRequest{}); err == nil {
		t.Fatal("create after close succeeded")
	}
}

func TestShutdownTimeoutMarksBlockedRecordingsIncomplete(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.ChunkEvents = 1 })
	release := e.faulty.blockAppends()
	defer release()
	info, p := e.create(session.CreateRequest{})
	p.Emit("x")
	waitFor(t, "blocked append", func() bool { return e.faulty.blocked.Load() > 0 })
	e.terminate(info.PublicID)

	closed := make(chan error, 1)
	go func() { closed <- e.svc.Close(context.Background()) }()
	waitFor(t, "shutdown timer", func() bool { return e.timers.pending(shutdownTimeout) == 1 })
	select {
	case err := <-closed:
		t.Fatalf("Close returned before its timeout: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	e.timers.fire(shutdownTimeout)
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("Close reported success for an unfinished recording")
		}
	case <-time.After(waitTimeout):
		t.Fatal("Close did not return after its timeout")
	}
	rec := e.recordingOf(info.PublicID)
	if rec.Status != store.RecordingIncomplete || rec.FailureCode != recording.CodeShutdownTimeout {
		t.Fatalf("recording = %+v", rec)
	}
}

func TestNewMarksInterruptedRecordings(t *testing.T) {
	e := newEnv(t, nil)
	info, _ := e.create(session.CreateRequest{})
	rec := e.recordingOf(info.PublicID)
	if _, err := recording.New(context.Background(), recording.Config{Store: e.db, Now: e.clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.db.Recording(context.Background(), rec.PublicID)
	if got.Status != store.RecordingIncomplete || got.FailureCode != recording.CodeInterrupted {
		t.Fatalf("interrupted recording = %+v", got)
	}
	if audits := e.auditEvents("recording.incomplete"); len(audits) != 1 || audits[0].Details["code"] != "interrupted" {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestConfigValidation(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, cfg := range map[string]recording.Config{
		"no store":        {},
		"negative queue":  {Store: db, QueueBytes: -1},
		"huge chunk":      {Store: db, ChunkBytes: 2 << 20},
		"tiny queue":      {Store: db, QueueBytes: 4 << 10},
		"max below chunk": {Store: db, ChunkBytes: 64 << 10, MaxRecordingBytes: 1024},
	} {
		if _, err := recording.New(context.Background(), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLargeOutputIsSplitIntoBoundedEvents(t *testing.T) {
	e := newEnv(t, nil)
	info, p := e.create(session.CreateRequest{})
	big := strings.Repeat("q", 40<<10)
	p.Emit(big)
	e.terminate(info.PublicID)
	rec := e.waitStatus(info.PublicID, store.RecordingComplete)
	var joined strings.Builder
	for _, ev := range allEvents(t, e.svc, rec.PublicID) {
		if ev.Kind == recording.KindOutput {
			if len(ev.Output) > recording.MaxOutputEventBytes {
				t.Fatalf("output event of %d bytes", len(ev.Output))
			}
			joined.Write(ev.Output)
		}
	}
	if joined.String() != big {
		t.Fatalf("reassembled %d bytes, want %d", joined.Len(), len(big))
	}
}

func TestPresenceForUnrecordedTerminalsIsIgnored(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.PresenceChanged("nobody", collab.Event{Type: collab.EventParticipantJoined, Participant: &collab.ParticipantInfo{ID: "x", Role: collab.RoleOwner}})
}

func TestExitCodeIsRecorded(t *testing.T) {
	e := newEnv(t, nil)
	info, p := e.create(session.CreateRequest{})
	p.Exit(pty.ExitStatus{Code: 7})
	rec := e.waitStatus(info.PublicID, store.RecordingComplete)
	events := allEvents(t, e.svc, rec.PublicID)
	last := events[len(events)-1]
	if last.Lifecycle == nil || last.Lifecycle.State != "exited" || last.Lifecycle.ExitCode == nil || *last.Lifecycle.ExitCode != 7 {
		t.Fatalf("final event = %+v", last)
	}
}
