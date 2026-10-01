package recording_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// pendingStatus returns the status a new watcher of terminalID receives
// immediately, if any.
func pendingStatus(svc *recording.Service, terminalID string) (recording.Status, bool) {
	statuses, cancel := svc.Watch(terminalID)
	defer cancel()
	select {
	case st := <-statuses:
		return st, true
	default:
		return recording.Status{}, false
	}
}

func installTrigger(t *testing.T, db *store.Store, name, sql string) {
	t.Helper()
	if _, err := db.DB().Exec(`CREATE TRIGGER ` + name + ` ` + sql); err != nil {
		t.Fatal(err)
	}
}

func allRecordings(t *testing.T, db *store.Store, terminalID string) []store.Recording {
	t.Helper()
	recs, err := db.Recordings(context.Background(), store.RecordingFilter{TerminalID: terminalID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// assertStartFailure checks the owner-visible warning and the persisted
// incomplete metadata of a recording that could not start, and that the
// live session is unaffected and nothing reports success.
func assertStartFailure(t *testing.T, e *env) {
	t.Helper()
	info, p := e.create(session.CreateRequest{})
	st, ok := pendingStatus(e.svc, info.PublicID)
	if !ok || st.RecordingID == "" || st.TerminalID != info.PublicID || st.State != store.RecordingIncomplete ||
		st.Code != recording.CodeStorage {
		t.Fatalf("pending status = %+v, %v", st, ok)
	}

	sub := subscribe(t, e.manager, info.PublicID)
	p.Emit("still live")
	if out := nextOutput(t, sub); out != "still live" {
		t.Fatalf("output = %q", out)
	}

	rec := e.waitRecording(info.PublicID, func(r store.Recording) bool { return r.Status == store.RecordingIncomplete })
	if rec.PublicID != st.RecordingID || rec.FailureCode != recording.CodeStorage || rec.ChunkCount != 0 ||
		rec.EventCount != 0 || rec.RetainUntil.IsZero() {
		t.Fatalf("persisted failure = %+v", rec)
	}
	audits := e.auditEvents("recording.incomplete")
	if len(audits) != 1 {
		t.Fatalf("incomplete audits = %+v", audits)
	}
	var keys []string
	for k := range audits[0].Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "code,recordingId,terminalId" || audits[0].Details["code"] != recording.CodeStorage ||
		audits[0].Details["recordingId"] != st.RecordingID {
		t.Fatalf("incomplete audit details = %v", audits[0].Details)
	}

	p.Exit(pty.ExitStatus{Code: 0})
	waitFor(t, "warning cleared after the session ended", func() bool {
		_, ok := pendingStatus(e.svc, info.PublicID)
		return !ok
	})
	if recs := allRecordings(t, e.db, info.PublicID); len(recs) != 1 || recs[0].Status != store.RecordingIncomplete {
		t.Fatalf("recordings after exit = %+v", recs)
	}
	if n := len(e.auditEvents("recording.started")) + len(e.auditEvents("recording.completed")); n != 0 {
		t.Fatalf("a failed recording reported %d start/complete audits", n)
	}
}

func TestBeginCreateFailureWarnsOwnersAndPersistsIncomplete(t *testing.T) {
	e := newEnv(t, nil)
	installTrigger(t, e.db, "fail_recording_create", `BEFORE INSERT ON recordings WHEN NEW.status = 'recording'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error at /var/lib/webpty/secret.db'); END`)
	assertStartFailure(t, e)
}

func TestBeginAuditFailureWarnsOwnersAndPersistsIncomplete(t *testing.T) {
	e := newEnv(t, nil)
	installTrigger(t, e.db, "fail_recording_audit", `BEFORE INSERT ON audit_events WHEN NEW.event_type = 'recording.started'
		BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`)
	assertStartFailure(t, e)
}

func TestBeginWithBlockedStoreIsBoundedAndRecoversLater(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.StartTimeout = 50 * time.Millisecond })
	release := e.faulty.blockTx()
	t.Cleanup(release)

	started := time.Now()
	info, p := e.create(session.CreateRequest{})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Create took %v with a blocked store", elapsed)
	}
	st, ok := pendingStatus(e.svc, info.PublicID)
	if !ok || st.Code != recording.CodeStorage {
		t.Fatalf("pending status = %+v, %v", st, ok)
	}
	sub := subscribe(t, e.manager, info.PublicID)
	p.Emit("live")
	if out := nextOutput(t, sub); out != "live" {
		t.Fatalf("output = %q", out)
	}
	if recs := allRecordings(t, e.db, info.PublicID); len(recs) != 0 {
		t.Fatalf("recordings while the store is blocked = %+v", recs)
	}

	release()
	rec := e.waitRecording(info.PublicID, func(r store.Recording) bool { return r.Status == store.RecordingIncomplete })
	if rec.PublicID != st.RecordingID || rec.FailureCode != recording.CodeStorage {
		t.Fatalf("recovered failure = %+v", rec)
	}
	if again, ok := pendingStatus(e.svc, info.PublicID); !ok || again != st {
		t.Fatalf("status after recovery = %+v, %v; want %+v", again, ok, st)
	}
}

func TestBeginFailureWithStorageDownKeepsWarningWithoutFalseSuccess(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) {
		c.StartTimeout = 20 * time.Millisecond
		c.StoreTimeout = 50 * time.Millisecond
	})
	release := e.faulty.blockTx()
	t.Cleanup(release)
	info, p := e.create(session.CreateRequest{})
	// Both the start and the recovery attempt give up on the blocked store.
	waitFor(t, "start and recovery attempts to time out", func() bool { return e.faulty.abandonedTx.Load() == 2 })
	if st, ok := pendingStatus(e.svc, info.PublicID); !ok || st.Code != recording.CodeStorage {
		t.Fatalf("pending status = %+v, %v", st, ok)
	}
	p.Exit(pty.ExitStatus{Code: 0})
	waitFor(t, "warning cleared after the session ended", func() bool {
		_, ok := pendingStatus(e.svc, info.PublicID)
		return !ok
	})
	release()
	if recs := allRecordings(t, e.db, info.PublicID); len(recs) != 0 {
		t.Fatalf("recordings = %+v, want none", recs)
	}
	if audits := e.auditEvents("recording.started"); len(audits) != 0 {
		t.Fatalf("started audits = %+v", audits)
	}
}

func TestBeginFailureWarningIsClearedOnClose(t *testing.T) {
	e := newEnv(t, nil)
	installTrigger(t, e.db, "fail_recording_create", `BEFORE INSERT ON recordings WHEN NEW.status = 'recording'
		BEGIN SELECT RAISE(ABORT, 'no'); END`)
	info, _ := e.create(session.CreateRequest{})
	if _, ok := pendingStatus(e.svc, info.PublicID); !ok {
		t.Fatal("no pending status")
	}
	e.waitStatus(info.PublicID, store.RecordingIncomplete)
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := e.svc.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if st, ok := pendingStatus(e.svc, info.PublicID); ok {
		t.Fatalf("status after Close = %+v", st)
	}
}

func TestCorruptionIsMarkedDurablyAfterTheRequestIsCancelled(t *testing.T) {
	for name, call := range map[string]func(context.Context, *env, string) error{
		"events": func(ctx context.Context, e *env, id string) error {
			_, err := e.svc.Events(ctx, id, recording.Query{Limit: 100}, "")
			return err
		},
		"export": func(ctx context.Context, e *env, id string) error {
			_, err := e.svc.PrepareExport(ctx, id, "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			rec := recordSample(t, e)
			dropChunkGuards(t, e.db)
			rewrite(t, e.db, rec.PublicID, 0, `"seq":3,`, `"seq":9,`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// The client goes away right after the corrupt chunk is read.
			e.faulty.mu.Lock()
			e.faulty.afterChunks = cancel
			e.faulty.mu.Unlock()
			defer func() {
				e.faulty.mu.Lock()
				e.faulty.afterChunks = nil
				e.faulty.mu.Unlock()
			}()

			if err := call(ctx, e, rec.PublicID); !errors.Is(err, recording.ErrCorrupt) {
				t.Fatalf("err = %v, want corrupt", err)
			}
			got, err := e.db.Recording(context.Background(), rec.PublicID)
			if err != nil || got.Status != store.RecordingIncomplete || got.FailureCode != recording.CodeCorrupt {
				t.Fatalf("recording after cancelled request = %+v, %v", got, err)
			}
			if audits := e.auditEvents("recording.incomplete"); len(audits) != 1 || audits[0].Details["code"] != recording.CodeCorrupt {
				t.Fatalf("incomplete audits = %+v", audits)
			}
		})
	}
}

// A start whose commit succeeded but was reported failed must not leave a
// phantom active recording or a duplicate: the stored row becomes the
// incomplete failure the owners were warned about.
func TestBeginWithAmbiguousCommitMarksTheStoredRecordingIncomplete(t *testing.T) {
	e := newEnv(t, nil)
	e.faulty.ambiguousCommits.Store(1)
	info, _ := e.create(session.CreateRequest{})
	st, ok := pendingStatus(e.svc, info.PublicID)
	if !ok || st.Code != recording.CodeStorage {
		t.Fatalf("pending status = %+v, %v", st, ok)
	}
	rec := e.waitRecording(info.PublicID, func(r store.Recording) bool { return r.Status == store.RecordingIncomplete })
	if rec.PublicID != st.RecordingID || rec.FailureCode != recording.CodeStorage {
		t.Fatalf("recording = %+v", rec)
	}
	if recs := allRecordings(t, e.db, info.PublicID); len(recs) != 1 {
		t.Fatalf("recordings = %+v, want exactly one", recs)
	}
	if audits := e.auditEvents("recording.incomplete"); len(audits) != 1 {
		t.Fatalf("incomplete audits = %+v", audits)
	}
}
