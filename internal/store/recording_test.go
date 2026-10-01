package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

var recordingStart = time.Unix(1_700_000_000, 0)

func seedTerminal(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if err := s.CreateTerminalSession(context.Background(), store.TerminalSession{
		PublicID: id, State: store.TerminalRunning, Command: "/bin/sh", Rows: 24, Cols: 80,
		CreatedAt: recordingStart, LastActivityAt: recordingStart,
	}); err != nil {
		t.Fatal(err)
	}
}

func createRecording(t *testing.T, s *store.Store, terminalID, id string) {
	t.Helper()
	err := s.WithTx(context.Background(), func(tx *store.Tx) error {
		return tx.CreateRecording(context.Background(), store.Recording{
			PublicID: id, TerminalID: terminalID, FormatVersion: 1, Codec: "gzip",
			StartedAt: recordingStart, Rows: 24, Cols: 80,
		})
	})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}
}

func chunk(index, firstSeq, lastSeq, firstMS, lastMS int64, data string) store.RecordingChunk {
	sum := sha256.Sum256([]byte(data))
	return store.RecordingChunk{
		Index: index, FirstSeq: firstSeq, LastSeq: lastSeq, FirstOffsetMS: firstMS, LastOffsetMS: lastMS,
		EventCount: lastSeq - firstSeq + 1, UncompressedBytes: int64(len(data)) * 2, Checksum: sum[:],
		Codec: "gzip", Data: []byte(data),
	}
}

func appendChunk(t *testing.T, s *store.Store, id string, c store.RecordingChunk) {
	t.Helper()
	if err := s.AppendRecordingChunk(context.Background(), id, c, recordingStart); err != nil {
		t.Fatalf("AppendRecordingChunk(%d): %v", c.Index, err)
	}
}

func finish(t *testing.T, s *store.Store, id string, end store.RecordingEnd) store.Recording {
	t.Helper()
	var rec store.Recording
	err := s.WithTx(context.Background(), func(tx *store.Tx) error {
		var err error
		rec, err = tx.FinishRecording(context.Background(), id, end)
		return err
	})
	if err != nil {
		t.Fatalf("FinishRecording: %v", err)
	}
	return rec
}

func completeEnd(at time.Time) store.RecordingEnd {
	return store.RecordingEnd{Status: store.RecordingComplete, EndedAt: at, RetainUntil: at.Add(time.Hour)}
}

func chunkRows(t *testing.T, s *store.Store, id string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM recording_chunks c JOIN recordings r ON r.id = c.recording_id
		WHERE r.public_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecordingSchemaHasIndexes(t *testing.T) {
	s := openTestStore(t)
	for _, index := range []string{"recordings_terminal", "recordings_started", "recordings_retention", "recording_chunks_offset"} {
		var name string
		if err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&name); err != nil {
			t.Errorf("index %q missing: %v", index, err)
		}
	}
}

func TestRecordingMigrationRetiresLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webpty.db")
	ctx := context.Background()
	if err := store.MigrateTo(ctx, path, 4); err != nil {
		t.Fatal(err)
	}
	raw, err := store.OpenRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO terminal_sessions (id, public_id, state, created_at, last_activity_at) VALUES (1, 'term', 'exited', 1, 1)`,
		`INSERT INTO recordings (id, terminal_session_id, started_at, ended_at) VALUES (7, 1, 10, 20)`,
		`INSERT INTO recording_chunks (recording_id, sequence, data) VALUES (7, 0, x'00')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	var status, code string
	var chunks int
	if err := s.DB().QueryRow(`SELECT status, failure_code, chunk_count FROM recordings WHERE id = 7`).Scan(&status, &code, &chunks); err != nil {
		t.Fatal(err)
	}
	if status != "incomplete" || code != "legacy_format" || chunks != 0 {
		t.Fatalf("legacy recording = %s/%s/%d", status, code, chunks)
	}
	var left int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM recording_chunks`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("legacy chunks kept: %d", left)
	}
}

func TestRecordingLifecycleTracksCounters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")

	rec, err := s.Recording(ctx, "rec")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != store.RecordingActive || rec.TerminalID != "term" || rec.Rows != 24 || !rec.EndedAt.IsZero() {
		t.Fatalf("new recording = %+v", rec)
	}

	appendChunk(t, s, "rec", chunk(0, 1, 3, 0, 40, "aaaa"))
	appendChunk(t, s, "rec", chunk(1, 4, 4, 40, 90, "bb"))
	end := recordingStart.Add(time.Second)
	rec = finish(t, s, "rec", store.RecordingEnd{Status: store.RecordingComplete, EndedAt: end, RetainUntil: end.Add(time.Hour)})
	want := store.Recording{
		PublicID: "rec", TerminalID: "term", Status: store.RecordingComplete, FormatVersion: 1, Codec: "gzip",
		StartedAt: recordingStart, EndedAt: end, DurationMS: 90, Rows: 24, Cols: 80, EventCount: 4, ChunkCount: 2,
		CompressedBytes: 6, UncompressedBytes: 12, RetainUntil: end.Add(time.Hour), UpdatedAt: end,
	}
	if fmt.Sprintf("%+v", rec) != fmt.Sprintf("%+v", want) {
		t.Fatalf("finished = %+v\nwant       %+v", rec, want)
	}
	if err := s.AppendRecordingChunk(ctx, "rec", chunk(2, 5, 5, 90, 90, "c"), end); !errors.Is(err, store.ErrRecordingNotActive) {
		t.Fatalf("append after finish err = %v", err)
	}
	err = s.WithTx(ctx, func(tx *store.Tx) error {
		_, err := tx.FinishRecording(ctx, "rec", completeEnd(end))
		return err
	})
	if !errors.Is(err, store.ErrRecordingNotActive) {
		t.Fatalf("second finish err = %v", err)
	}
}

func TestCreateRecordingRequiresTerminal(t *testing.T) {
	s := openTestStore(t)
	err := s.WithTx(context.Background(), func(tx *store.Tx) error {
		return tx.CreateRecording(context.Background(), store.Recording{PublicID: "r", TerminalID: "missing",
			FormatVersion: 1, Codec: "gzip", StartedAt: recordingStart, Rows: 1, Cols: 1})
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAppendRecordingChunkRejectsGapsAtomically(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")
	appendChunk(t, s, "rec", chunk(0, 1, 2, 0, 10, "aa"))

	for name, bad := range map[string]store.RecordingChunk{
		"index gap":      chunk(2, 3, 3, 10, 10, "x"),
		"sequence gap":   chunk(1, 5, 5, 10, 10, "x"),
		"offset regress": chunk(1, 3, 3, 5, 10, "x"),
		"bad checksum":   func() store.RecordingChunk { c := chunk(1, 3, 3, 10, 10, "x"); c.Checksum = []byte{1}; return c }(),
		"bad count":      func() store.RecordingChunk { c := chunk(1, 3, 4, 10, 10, "x"); c.EventCount = 1; return c }(),
	} {
		if err := s.AppendRecordingChunk(ctx, "rec", bad, recordingStart); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	rec, _ := s.Recording(ctx, "rec")
	if rec.ChunkCount != 1 || rec.EventCount != 2 || rec.CompressedBytes != 2 || chunkRows(t, s, "rec") != 1 {
		t.Fatalf("rejected appends changed the recording: %+v rows=%d", rec, chunkRows(t, s, "rec"))
	}
}

func TestAppendRecordingChunkRollsBackCountersWhenInsertFails(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_chunks BEFORE INSERT ON recording_chunks BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRecordingChunk(ctx, "rec", chunk(0, 1, 1, 0, 0, "a"), recordingStart); err == nil {
		t.Fatal("append succeeded")
	}
	rec, _ := s.Recording(ctx, "rec")
	if rec.ChunkCount != 0 || rec.EventCount != 0 || rec.CompressedBytes != 0 {
		t.Fatalf("counters moved without a chunk: %+v", rec)
	}
}

func TestDeleteRecordingTombstonesAndIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")
	appendChunk(t, s, "rec", chunk(0, 1, 1, 0, 0, "a"))

	del := func(id string) (store.Recording, bool, error) {
		var rec store.Recording
		var changed bool
		err := s.WithTx(ctx, func(tx *store.Tx) error {
			var err error
			rec, changed, err = tx.DeleteRecording(ctx, id, recordingStart.Add(time.Hour))
			return err
		})
		return rec, changed, err
	}
	if _, _, err := del("rec"); !errors.Is(err, store.ErrRecordingActive) {
		t.Fatalf("delete active err = %v", err)
	}
	if chunkRows(t, s, "rec") != 1 {
		t.Fatal("active delete removed chunks")
	}
	finish(t, s, "rec", completeEnd(recordingStart.Add(time.Second)))

	rec, changed, err := del("rec")
	if err != nil || !changed {
		t.Fatalf("delete = %v changed=%v", err, changed)
	}
	if rec.Status != store.RecordingDeleted || !rec.DeletedAt.Equal(recordingStart.Add(time.Hour)) || rec.ChunkCount != 1 {
		t.Fatalf("tombstone = %+v", rec)
	}
	if chunkRows(t, s, "rec") != 0 {
		t.Fatal("chunks survived deletion")
	}
	if _, changed, err := del("rec"); err != nil || changed {
		t.Fatalf("second delete = %v changed=%v", err, changed)
	}
	if _, _, err := del("missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE recordings SET status = 'complete', deleted_at = NULL WHERE public_id = 'rec'`); err == nil {
		t.Fatal("tombstone was resurrected")
	}
	err = s.WithTx(ctx, func(tx *store.Tx) error {
		changed, err := tx.MarkRecordingIncomplete(ctx, "rec", "corrupt", recordingStart, recordingStart)
		if changed {
			t.Error("tombstone marked incomplete")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO recording_chunks (recording_id, chunk_index, first_seq, last_seq, first_offset_ms,
		last_offset_ms, event_count, uncompressed_bytes, checksum, codec, data)
		SELECT id, 5, 1, 1, 0, 0, 1, 1, zeroblob(32), 'gzip', x'00' FROM recordings WHERE public_id = 'rec'`); err == nil {
		t.Fatal("chunk inserted into a tombstone")
	}
}

func TestDeleteRecordingRollsBackWithTransaction(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")
	appendChunk(t, s, "rec", chunk(0, 1, 1, 0, 0, "a"))
	finish(t, s, "rec", completeEnd(recordingStart))
	sentinel := errors.New("audit failed")
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		if _, _, err := tx.DeleteRecording(ctx, "rec", recordingStart); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	rec, _ := s.Recording(ctx, "rec")
	if rec.Status != store.RecordingComplete || chunkRows(t, s, "rec") != 1 {
		t.Fatalf("rolled-back delete left %+v rows=%d", rec, chunkRows(t, s, "rec"))
	}
}

func TestExpiredRecordingsSkipsActiveAndDeleted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	for i := 0; i < 5; i++ {
		createRecording(t, s, "term", fmt.Sprintf("old-%d", i))
		finish(t, s, fmt.Sprintf("old-%d", i), store.RecordingEnd{Status: store.RecordingComplete,
			EndedAt: recordingStart, RetainUntil: recordingStart.Add(time.Minute)})
	}
	createRecording(t, s, "term", "active")
	createRecording(t, s, "term", "fresh")
	finish(t, s, "fresh", store.RecordingEnd{Status: store.RecordingIncomplete, FailureCode: "queue_overflow",
		EndedAt: recordingStart, RetainUntil: recordingStart.Add(time.Hour)})

	now := recordingStart.Add(2 * time.Minute)
	var ids []string
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		ids, err = tx.ExpiredRecordings(ctx, now, 3)
		return err
	})
	if err != nil || len(ids) != 3 || ids[0] != "old-0" {
		t.Fatalf("first batch = %v, %v", ids, err)
	}
	err = s.WithTx(ctx, func(tx *store.Tx) error {
		for _, id := range ids {
			if _, _, err := tx.DeleteRecording(ctx, id, now); err != nil {
				return err
			}
		}
		ids, err = tx.ExpiredRecordings(ctx, now, 10)
		return err
	})
	if err != nil || fmt.Sprint(ids) != "[old-3 old-4]" {
		t.Fatalf("second batch = %v, %v", ids, err)
	}
}

func TestMarkRecordingIncompleteTransitions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "active")
	createRecording(t, s, "term", "done")
	finish(t, s, "done", completeEnd(recordingStart.Add(time.Second)))

	mark := func(id string) bool {
		var changed bool
		err := s.WithTx(ctx, func(tx *store.Tx) error {
			var err error
			changed, err = tx.MarkRecordingIncomplete(ctx, id, "corrupt", recordingStart.Add(time.Minute), recordingStart.Add(time.Hour))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !mark("active") || !mark("done") || mark("done") {
		t.Fatal("unexpected transition result")
	}
	active, _ := s.Recording(ctx, "active")
	if active.Status != store.RecordingIncomplete || active.FailureCode != "corrupt" ||
		!active.EndedAt.Equal(recordingStart.Add(time.Minute)) || !active.RetainUntil.Equal(recordingStart.Add(time.Hour)) {
		t.Fatalf("active marked = %+v", active)
	}
	done, _ := s.Recording(ctx, "done")
	if done.Status != store.RecordingIncomplete || !done.EndedAt.Equal(recordingStart.Add(time.Second)) ||
		!done.RetainUntil.Equal(recordingStart.Add(time.Second+time.Hour)) {
		t.Fatalf("complete marked = %+v", done)
	}
}

func TestFailInterruptedRecordings(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "a")
	appendChunk(t, s, "a", chunk(0, 1, 1, 0, 70, "a"))
	createRecording(t, s, "term", "b")
	finish(t, s, "b", completeEnd(recordingStart))
	now := recordingStart.Add(time.Hour)
	var ids []string
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		ids, err = tx.FailInterruptedRecordings(ctx, now, now.Add(time.Hour))
		return err
	})
	if err != nil || fmt.Sprint(ids) != "[a]" {
		t.Fatalf("interrupted = %v, %v", ids, err)
	}
	a, _ := s.Recording(ctx, "a")
	if a.Status != store.RecordingIncomplete || a.FailureCode != "interrupted" || a.DurationMS != 70 || !a.EndedAt.Equal(now) {
		t.Fatalf("interrupted recording = %+v", a)
	}
}

func TestRecordingsListNewestFirstWithFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "one")
	seedTerminal(t, s, "two")
	createRecording(t, s, "one", "r1")
	createRecording(t, s, "two", "r2")
	createRecording(t, s, "one", "r3")

	all, err := s.Recordings(ctx, store.RecordingFilter{Limit: 10})
	if err != nil || len(all) != 3 || all[0].PublicID != "r3" || all[2].PublicID != "r1" {
		t.Fatalf("all = %v, %v", all, err)
	}
	one, err := s.Recordings(ctx, store.RecordingFilter{TerminalID: "one", Limit: 1})
	if err != nil || len(one) != 1 || one[0].PublicID != "r3" {
		t.Fatalf("filtered = %v, %v", one, err)
	}
	if _, err := s.Recording(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestRecordingChunksSeeksWithPreviousChunk(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")
	appendChunk(t, s, "rec", chunk(0, 1, 2, 0, 100, "c0"))
	appendChunk(t, s, "rec", chunk(1, 3, 4, 100, 200, "c1"))
	appendChunk(t, s, "rec", chunk(2, 5, 6, 250, 300, "c2"))
	appendChunk(t, s, "rec", chunk(3, 7, 8, 300, 400, "c3"))

	page, err := s.RecordingChunks(ctx, "rec", store.ChunkQuery{AfterSeq: 0, AfterMS: 150, MaxChunks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.Recording.ChunkCount != 4 || page.Previous == nil || page.Previous.Index != 0 || page.Previous.Data != nil {
		t.Fatalf("previous = %+v recording %+v", page.Previous, page.Recording)
	}
	if len(page.Chunks) != 2 || page.Chunks[0].Index != 1 || !bytes.Equal(page.Chunks[1].Data, []byte("c2")) {
		t.Fatalf("chunks = %+v", page.Chunks)
	}

	page, err = s.RecordingChunks(ctx, "rec", store.ChunkQuery{AfterSeq: 6, AfterMS: 0, MaxChunks: 10})
	if err != nil || len(page.Chunks) != 1 || page.Chunks[0].Index != 3 || page.Previous.Index != 2 {
		t.Fatalf("seq seek = %+v, %v", page, err)
	}
	page, err = s.RecordingChunks(ctx, "rec", store.ChunkQuery{AfterSeq: 0, AfterMS: 0, MaxChunks: 10})
	if err != nil || page.Previous != nil || len(page.Chunks) != 4 {
		t.Fatalf("from start = %+v, %v", page, err)
	}
	page, err = s.RecordingChunks(ctx, "rec", store.ChunkQuery{AfterSeq: 0, AfterMS: 1000, MaxChunks: 10})
	if err != nil || len(page.Chunks) != 0 {
		t.Fatalf("past end = %+v, %v", page, err)
	}
	if _, err := s.RecordingChunks(ctx, "missing", store.ChunkQuery{AfterSeq: 0, AfterMS: 0, MaxChunks: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestBackupIsConsistentWithConcurrentRecordingWrites(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "live")
	createRecording(t, s, "term", "gone")
	appendChunk(t, s, "gone", chunk(0, 1, 1, 0, 0, "g"))
	finish(t, s, "gone", completeEnd(recordingStart))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(0); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.AppendRecordingChunk(ctx, "live", chunk(i, i+1, i+1, i, i, "x"), recordingStart); err != nil {
				t.Errorf("append: %v", err)
				return
			}
		}
	}()
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		_, _, err := tx.DeleteRecording(ctx, "gone", recordingStart)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	backupErr := s.Backup(ctx, backup)
	close(stop)
	wg.Wait()
	if backupErr != nil {
		t.Fatalf("Backup: %v", backupErr)
	}
	if err := s.Backup(ctx, backup); err == nil {
		t.Fatal("backup overwrote an existing file")
	}

	copied, err := store.Open(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	assertCountersMatchChunks(t, copied.DB())
	gone, err := copied.Recording(ctx, "gone")
	if err != nil || gone.Status != store.RecordingDeleted || chunkRows(t, copied, "gone") != 0 {
		t.Fatalf("backup tombstone = %+v, %v", gone, err)
	}
}

func assertCountersMatchChunks(t *testing.T, db *sql.DB) {
	t.Helper()
	var mismatched int
	err := db.QueryRow(`SELECT COUNT(*) FROM recordings r WHERE r.status != 'deleted' AND (
		r.chunk_count != (SELECT COUNT(*) FROM recording_chunks c WHERE c.recording_id = r.id) OR
		r.event_count != (SELECT COALESCE(SUM(event_count), 0) FROM recording_chunks c WHERE c.recording_id = r.id) OR
		r.compressed_bytes != (SELECT COALESCE(SUM(length(data)), 0) FROM recording_chunks c WHERE c.recording_id = r.id))`).Scan(&mismatched)
	if err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("%d recordings have counters that disagree with their chunks", mismatched)
	}
}

func TestConcurrentDeleteAndAppendKeepInvariants(t *testing.T) {
	for round := 0; round < 5; round++ {
		s := openTestStore(t)
		ctx := context.Background()
		seedTerminal(t, s, "term")
		createRecording(t, s, "term", "rec")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := int64(0); i < 50; i++ {
				err := s.AppendRecordingChunk(ctx, "rec", chunk(i, i+1, i+1, i, i, "x"), recordingStart)
				if errors.Is(err, store.ErrRecordingNotActive) {
					return
				}
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			_ = s.WithTx(ctx, func(tx *store.Tx) error {
				_, err := tx.FinishRecording(ctx, "rec", completeEnd(recordingStart))
				return err
			})
			for {
				var err error
				err = s.WithTx(ctx, func(tx *store.Tx) error {
					_, _, err := tx.DeleteRecording(ctx, "rec", recordingStart)
					return err
				})
				if err == nil {
					return
				}
				if !errors.Is(err, store.ErrRecordingActive) {
					t.Errorf("delete: %v", err)
					return
				}
			}
		}()
		wg.Wait()
		rec, _ := s.Recording(ctx, "rec")
		if rec.Status != store.RecordingDeleted || chunkRows(t, s, "rec") != 0 {
			t.Fatalf("round %d: %+v rows=%d", round, rec, chunkRows(t, s, "rec"))
		}
	}
}

func TestCreateFailedRecordingStoresIncompleteMetadataOnly(t *testing.T) {
	s := openTestStore(t)
	seedTerminal(t, s, "term")
	ctx := context.Background()
	failed := store.Recording{PublicID: "rec", TerminalID: "term", FormatVersion: 1, Codec: "gzip",
		StartedAt: recordingStart, Rows: 24, Cols: 80, FailureCode: "storage_error",
		RetainUntil: recordingStart.Add(time.Hour)}
	if err := s.WithTx(ctx, func(tx *store.Tx) error { return tx.CreateFailedRecording(ctx, failed) }); err != nil {
		t.Fatalf("CreateFailedRecording: %v", err)
	}
	rec, err := s.Recording(ctx, "rec")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != store.RecordingIncomplete || rec.FailureCode != "storage_error" || !rec.EndedAt.Equal(recordingStart) ||
		!rec.RetainUntil.Equal(recordingStart.Add(time.Hour)) || rec.EventCount != 0 || rec.ChunkCount != 0 || rec.DurationMS != 0 {
		t.Fatalf("failed recording = %+v", rec)
	}
	if err := s.AppendRecordingChunk(ctx, "rec", chunk(0, 1, 1, 0, 0, "x"), recordingStart); !errors.Is(err, store.ErrRecordingNotActive) {
		t.Fatalf("append to failed recording err = %v, want ErrRecordingNotActive", err)
	}
	if err := s.WithTx(ctx, func(tx *store.Tx) error { return tx.CreateFailedRecording(ctx, failed) }); err == nil {
		t.Fatal("duplicate failed recording accepted")
	}

	for name, rec := range map[string]store.Recording{
		"missing terminal": {PublicID: "r2", TerminalID: "missing", FailureCode: "storage_error", StartedAt: recordingStart, RetainUntil: recordingStart},
		"no failure code":  {PublicID: "r3", TerminalID: "term", StartedAt: recordingStart, RetainUntil: recordingStart},
	} {
		err := s.WithTx(ctx, func(tx *store.Tx) error { return tx.CreateFailedRecording(ctx, rec) })
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name == "missing terminal" && !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}
