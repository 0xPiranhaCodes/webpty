package recording_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// recordSample records 1@0 running, 2@100 a, 3@100 b, 4@200 c, 5@300
// resize, 6@400 d, 7@400 terminated in chunks of four events.
func recordSample(t *testing.T, e *env) store.Recording {
	t.Helper()
	info, p := e.create(session.CreateRequest{})
	sub := subscribe(t, e.manager, info.PublicID)
	emit := func(advance time.Duration, data string) {
		e.clock.Advance(advance)
		p.Emit(data)
		nextOutput(t, sub)
	}
	emit(100*time.Millisecond, "a")
	emit(0, "b")
	emit(100*time.Millisecond, "c")
	e.clock.Advance(100 * time.Millisecond)
	if err := e.manager.Resize(info.PublicID, 40, 120); err != nil {
		t.Fatal(err)
	}
	emit(100*time.Millisecond, "d")
	e.terminate(info.PublicID)
	return e.waitStatus(info.PublicID, store.RecordingComplete)
}

func TestPlaybackSeeksDeterministicallyWithBoundedPages(t *testing.T) {
	e := newEnv(t, nil)
	rec := recordSample(t, e)
	ctx := context.Background()
	if rec.ChunkCount != 2 || rec.EventCount != 7 {
		t.Fatalf("recording = %+v", rec)
	}

	query := func(q recording.Query) ([]string, *recording.Cursor) {
		t.Helper()
		page, err := e.svc.Events(ctx, rec.PublicID, q, "192.0.2.1")
		if err != nil {
			t.Fatalf("Events(%+v): %v", q, err)
		}
		if page.Recording.PublicID != rec.PublicID {
			t.Fatalf("page recording = %+v", page.Recording)
		}
		return describe(page.Events), page.Next
	}
	events, next := query(recording.Query{AfterMS: 100, Limit: 2})
	if fmt.Sprint(events) != "[2@100 output:a 3@100 output:b]" || next == nil || *next != (recording.Cursor{AfterMS: 100, AfterSeq: 3}) {
		t.Fatalf("first page = %v next %+v", events, next)
	}
	again, againNext := query(recording.Query{AfterMS: 100, Limit: 2})
	if !reflect.DeepEqual(again, events) || *againNext != *next {
		t.Fatal("repeated query differs")
	}
	events, next = query(recording.Query{AfterMS: next.AfterMS, AfterSeq: next.AfterSeq, Limit: 2})
	if fmt.Sprint(events) != "[4@200 output:c 5@300 resize:40x120]" || next == nil || next.AfterSeq != 5 {
		t.Fatalf("second page = %v next %+v", events, next)
	}
	events, next = query(recording.Query{AfterMS: next.AfterMS, AfterSeq: next.AfterSeq, Limit: 2})
	if fmt.Sprint(events) != "[6@400 output:d 7@400 lifecycle:terminated:admin]" || next != nil {
		t.Fatalf("last page = %v next %+v", events, next)
	}
	if events, next = query(recording.Query{AfterMS: 250, Limit: 10}); len(events) != 3 || next != nil {
		t.Fatalf("seek to 250 = %v next %+v", events, next)
	}
	if events, next = query(recording.Query{AfterMS: 5000, Limit: 10}); len(events) != 0 || next != nil {
		t.Fatalf("seek past end = %v next %+v", events, next)
	}
	if events, _ = query(recording.Query{}); len(events) != 7 {
		t.Fatalf("default limit returned %v", events)
	}
	for _, bad := range []recording.Query{{AfterMS: -1}, {AfterSeq: -1}, {Limit: -1}, {Limit: recording.MaxPageEvents + 1}} {
		if _, err := e.svc.Events(ctx, rec.PublicID, bad, ""); !errors.Is(err, recording.ErrInvalidArgument) {
			t.Errorf("Events(%+v) err = %v", bad, err)
		}
	}
	if _, err := e.svc.Events(ctx, "missing", recording.Query{}, ""); !errors.Is(err, recording.ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	if audits := e.auditEvents("recording.playback"); len(audits) < 6 || audits[0].Details["recordingId"] != rec.PublicID {
		t.Fatalf("playback audits = %+v", audits)
	}
}

func TestPlaybackEventsMarshalWithBase64Output(t *testing.T) {
	e := newEnv(t, nil)
	rec := recordSample(t, e)
	page, err := e.svc.Events(context.Background(), rec.PublicID, recording.Query{Limit: 3}, "")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page.Events)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"seq":1,"offsetMs":0,"kind":"lifecycle","data":{"state":"running"}},` +
		`{"seq":2,"offsetMs":100,"kind":"output","data":{"data":"YQ=="}},` +
		`{"seq":3,"offsetMs":100,"kind":"output","data":{"data":"Yg=="}}]`
	if string(encoded) != want {
		t.Fatalf("json = %s\nwant   %s", encoded, want)
	}
}

func dropChunkGuards(t *testing.T, db *store.Store) {
	t.Helper()
	for _, trigger := range []string{"recording_chunks_are_immutable", "recording_chunks_delete_with_tombstone"} {
		if _, err := db.DB().Exec(`DROP TRIGGER ` + trigger); err != nil {
			t.Fatal(err)
		}
	}
}

func chunkPlain(t *testing.T, db *store.Store, recID string, index int) string {
	t.Helper()
	var data []byte
	if err := db.DB().QueryRow(`SELECT c.data FROM recording_chunks c JOIN recordings r ON r.id = c.recording_id
		WHERE r.public_id = ? AND c.chunk_index = ?`, recID, index).Scan(&data); err != nil {
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
	return string(plain)
}

func gzipped(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func setChunk(t *testing.T, db *store.Store, recID string, index int, data []byte, uncompressed int, fixChecksum bool) {
	t.Helper()
	sum := sha256.Sum256(data)
	query := `UPDATE recording_chunks SET data = ?1, uncompressed_bytes = ?2 WHERE chunk_index = ?4
		AND recording_id = (SELECT id FROM recordings WHERE public_id = ?5)`
	if fixChecksum {
		query = `UPDATE recording_chunks SET data = ?1, uncompressed_bytes = ?2, checksum = ?3 WHERE chunk_index = ?4
			AND recording_id = (SELECT id FROM recordings WHERE public_id = ?5)`
	}
	if _, err := db.DB().Exec(query, data, uncompressed, sum[:], index, recID); err != nil {
		t.Fatal(err)
	}
}

func rewrite(t *testing.T, db *store.Store, recID string, index int, old, replacement string) {
	t.Helper()
	plain := chunkPlain(t, db, recID, index)
	if !strings.Contains(plain, old) {
		t.Fatalf("chunk %d has no %q:\n%s", index, old, plain)
	}
	changed := []byte(strings.Replace(plain, old, replacement, 1))
	setChunk(t, db, recID, index, gzipped(t, changed), len(changed), true)
}

func TestCorruptionIsDetectedAndMarksRecordingIncomplete(t *testing.T) {
	cases := map[string]struct {
		code   string
		tamper func(t *testing.T, db *store.Store, id string)
	}{
		"checksum": {"checksum", func(t *testing.T, db *store.Store, id string) {
			if _, err := db.DB().Exec(`UPDATE recording_chunks SET data = CAST(data || x'00' AS BLOB) WHERE chunk_index = 1
				AND recording_id = (SELECT id FROM recordings WHERE public_id = ?)`, id); err != nil {
				t.Fatal(err)
			}
		}},
		"not gzip": {"decompress", func(t *testing.T, db *store.Store, id string) {
			setChunk(t, db, id, 0, []byte("definitely not gzip"), 100, true)
		}},
		"decompression bomb": {"decompress_limit", func(t *testing.T, db *store.Store, id string) {
			setChunk(t, db, id, 0, gzipped(t, make([]byte, 8<<20)), 100, true)
		}},
		"declared too large": {"decompress_limit", func(t *testing.T, db *store.Store, id string) {
			plain := chunkPlain(t, db, id, 0)
			setChunk(t, db, id, 0, gzipped(t, []byte(plain)), 64<<20, true)
		}},
		"version": {"version", func(t *testing.T, db *store.Store, id string) {
			rewrite(t, db, id, 0, `{"v":1,"seq":1,`, `{"v":2,"seq":1,`)
		}},
		"unknown field": {"decode", func(t *testing.T, db *store.Store, id string) {
			rewrite(t, db, id, 0, `{"v":1,"seq":1,`, `{"v":1,"token":"x","seq":1,`)
		}},
		"sequence": {"sequence", func(t *testing.T, db *store.Store, id string) {
			rewrite(t, db, id, 0, `"seq":3,`, `"seq":9,`)
		}},
		"offset": {"offset", func(t *testing.T, db *store.Store, id string) {
			rewrite(t, db, id, 0, `"seq":3,"ms":100`, `"seq":3,"ms":50`)
		}},
		"bad base64": {"decode", func(t *testing.T, db *store.Store, id string) {
			rewrite(t, db, id, 0, `{"data":"YQ=="}`, `{"data":"!!"}`)
		}},
		"missing chunk": {"chunk_order", func(t *testing.T, db *store.Store, id string) {
			if _, err := db.DB().Exec(`DELETE FROM recording_chunks WHERE chunk_index = 0
				AND recording_id = (SELECT id FROM recordings WHERE public_id = ?)`, id); err != nil {
				t.Fatal(err)
			}
		}},
		"truncated final chunk": {"count", func(t *testing.T, db *store.Store, id string) {
			if _, err := db.DB().Exec(`DELETE FROM recording_chunks WHERE chunk_index = 1
				AND recording_id = (SELECT id FROM recordings WHERE public_id = ?)`, id); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			rec := recordSample(t, e)
			dropChunkGuards(t, e.db)
			tc.tamper(t, e.db, rec.PublicID)

			page, err := e.svc.Events(context.Background(), rec.PublicID, recording.Query{Limit: 100}, "")
			var corrupt *recording.CorruptError
			if !errors.As(err, &corrupt) || !errors.Is(err, recording.ErrCorrupt) || corrupt.Code != tc.code {
				t.Fatalf("Events err = %v, want corrupt %q", err, tc.code)
			}
			if len(page.Events) != 0 {
				t.Fatalf("served %d events from a corrupt recording", len(page.Events))
			}
			got, _ := e.db.Recording(context.Background(), rec.PublicID)
			if got.Status != store.RecordingIncomplete || got.FailureCode != recording.CodeCorrupt {
				t.Fatalf("recording after corruption = %+v", got)
			}
			if audits := e.auditEvents("recording.incomplete"); len(audits) != 1 || audits[0].Details["code"] != "corrupt" {
				t.Fatalf("audits = %+v", audits)
			}
			if _, err := e.svc.PrepareExport(context.Background(), rec.PublicID, ""); !errors.Is(err, recording.ErrCorrupt) {
				t.Fatalf("export err = %v", err)
			}
			if audits := e.auditEvents("recording.incomplete"); len(audits) != 1 {
				t.Fatalf("corruption audited %d times", len(audits))
			}
		})
	}
}

func TestExportIsAsciicastV2WithTimingAndSize(t *testing.T) {
	e := newEnv(t, nil)
	info, p := e.create(session.CreateRequest{Rows: 24, Cols: 80})
	sub := subscribe(t, e.manager, info.PublicID)
	e.clock.Advance(100 * time.Millisecond)
	p.Emit("h\xc3")
	nextOutput(t, sub)
	e.clock.Advance(150 * time.Millisecond)
	p.Emit("\xa9llo")
	nextOutput(t, sub)
	if err := e.manager.Write(context.Background(), info.PublicID, []byte("typed-password")); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(250 * time.Millisecond)
	if err := e.manager.Resize(info.PublicID, 40, 120); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(1500 * time.Millisecond)
	p.Emit("<done>")
	nextOutput(t, sub)
	e.terminate(info.PublicID)
	rec := e.waitStatus(info.PublicID, store.RecordingComplete)

	export, err := e.svc.PrepareExport(context.Background(), rec.PublicID, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if export.Recording().PublicID != rec.PublicID {
		t.Fatalf("export recording = %+v", export.Recording())
	}
	var buf bytes.Buffer
	if err := export.WriteTo(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	var header map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header["version"] != float64(2) || header["width"] != float64(80) || header["height"] != float64(24) ||
		header["duration"] != 2.0 || header["timestamp"] != float64(1_700_000_000) || len(header) != 5 {
		t.Fatalf("header = %v", header)
	}
	var got []string
	for _, line := range lines[1:] {
		var event []any
		if err := json.Unmarshal([]byte(line), &event); err != nil || len(event) != 3 {
			t.Fatalf("event line %q: %v", line, err)
		}
		got = append(got, fmt.Sprintf("%v %v %v", event[0], event[1], event[2]))
	}
	want := []string{"0.1 o h", "0.25 o éllo", "0.5 r 120x40", "2 o <done>"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %q\nwant   %q", got, want)
	}
	if strings.Contains(buf.String(), "typed-password") || strings.Contains(buf.String(), "lifecycle") {
		t.Fatalf("export leaked input or metadata:\n%s", buf.String())
	}
	if audits := e.auditEvents("recording.exported"); len(audits) != 1 || audits[0].Details["events"] != "6" {
		t.Fatalf("export audits = %+v", audits)
	}
}

func TestExportRejectsActiveRecordings(t *testing.T) {
	e := newEnv(t, nil)
	info, _ := e.create(session.CreateRequest{})
	rec := e.recordingOf(info.PublicID)
	if _, err := e.svc.PrepareExport(context.Background(), rec.PublicID, ""); !errors.Is(err, recording.ErrActive) {
		t.Fatalf("export active err = %v", err)
	}
	if _, err := e.svc.Delete(context.Background(), rec.PublicID, ""); !errors.Is(err, recording.ErrActive) {
		t.Fatalf("delete active err = %v", err)
	}
	if page, err := e.svc.Events(context.Background(), rec.PublicID, recording.Query{}, ""); err != nil || page.Recording.Status != store.RecordingActive {
		t.Fatalf("playback of active recording = %+v, %v", page, err)
	}
}

func TestDeleteIsIdempotentAndDisablesPlayback(t *testing.T) {
	e := newEnv(t, nil)
	rec := recordSample(t, e)
	ctx := context.Background()
	deleted, err := e.svc.Delete(ctx, rec.PublicID, "192.0.2.1")
	if err != nil || deleted.Status != store.RecordingDeleted || deleted.DeletedAt.IsZero() {
		t.Fatalf("Delete = %+v, %v", deleted, err)
	}
	if again, err := e.svc.Delete(ctx, rec.PublicID, ""); err != nil || again.Status != store.RecordingDeleted {
		t.Fatalf("second Delete = %+v, %v", again, err)
	}
	if audits := e.auditEvents("recording.deleted"); len(audits) != 1 || audits[0].Details["recordingId"] != rec.PublicID {
		t.Fatalf("delete audits = %+v", audits)
	}
	if _, err := e.svc.Events(ctx, rec.PublicID, recording.Query{}, ""); !errors.Is(err, recording.ErrDeleted) {
		t.Fatalf("playback after delete err = %v", err)
	}
	if _, err := e.svc.PrepareExport(ctx, rec.PublicID, ""); !errors.Is(err, recording.ErrDeleted) {
		t.Fatalf("export after delete err = %v", err)
	}
	if tomb, err := e.svc.Recording(ctx, rec.PublicID); err != nil || tomb.Status != store.RecordingDeleted {
		t.Fatalf("tombstone = %+v, %v", tomb, err)
	}
	if _, err := e.svc.Delete(ctx, "missing", ""); !errors.Is(err, recording.ErrNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
}

func TestRetentionDeletesEligibleRecordingsInBatches(t *testing.T) {
	e := newEnv(t, nil)
	var old []string
	for i := 0; i < 3; i++ {
		info, _ := e.create(session.CreateRequest{})
		e.terminate(info.PublicID)
		old = append(old, e.waitStatus(info.PublicID, store.RecordingComplete).PublicID)
	}
	e.clock.Advance(2 * time.Hour)
	active, _ := e.create(session.CreateRequest{})
	fresh, _ := e.create(session.CreateRequest{})
	e.terminate(fresh.PublicID)
	e.waitStatus(fresh.PublicID, store.RecordingComplete)

	n, err := e.svc.RunRetention(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("RunRetention = %d, %v", n, err)
	}
	for _, id := range old {
		if rec, _ := e.db.Recording(context.Background(), id); rec.Status != store.RecordingDeleted {
			t.Fatalf("expired recording kept: %+v", rec)
		}
	}
	if rec := e.recordingOf(active.PublicID); rec.Status != store.RecordingActive {
		t.Fatalf("active recording = %+v", rec)
	}
	if rec := e.recordingOf(fresh.PublicID); rec.Status != store.RecordingComplete {
		t.Fatalf("fresh recording = %+v", rec)
	}
	if audits := e.auditEvents("recording.retention.deleted"); len(audits) != 3 {
		t.Fatalf("retention audits = %+v", audits)
	}
	if n, err := e.svc.RunRetention(context.Background()); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v", n, err)
	}
}

func TestConcurrentDeletionAndPlaybackNeverServesPartialOrMarksCorrupt(t *testing.T) {
	for round := 0; round < 3; round++ {
		e := newEnv(t, nil)
		rec := recordSample(t, e)
		ctx := context.Background()
		// Each round deletes after a different number of reads.
		var reads atomic.Int32
		deleteNow := make(chan struct{})
		var trigger sync.Once
		if round == 0 {
			trigger.Do(func() { close(deleteNow) })
		}
		e.faulty.setAfterChunks(func() {
			if reads.Add(1) >= int32(round*4) {
				trigger.Do(func() { close(deleteNow) })
			}
		})
		var wg sync.WaitGroup
		for reader := 0; reader < 3; reader++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 40; i++ {
					page, err := e.svc.Events(ctx, rec.PublicID, recording.Query{Limit: 100}, "")
					switch {
					case errors.Is(err, recording.ErrDeleted):
					case err != nil:
						t.Errorf("playback: %v", err)
						return
					case len(page.Events) != 7:
						t.Errorf("partial playback: %d events", len(page.Events))
						return
					}
					if export, err := e.svc.PrepareExport(ctx, rec.PublicID, ""); err == nil {
						if err := export.WriteTo(ctx, io.Discard); err != nil && !errors.Is(err, recording.ErrDeleted) {
							t.Errorf("export: %v", err)
						}
					} else if !errors.Is(err, recording.ErrDeleted) {
						t.Errorf("prepare export: %v", err)
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-deleteNow
			if _, err := e.svc.Delete(ctx, rec.PublicID, ""); err != nil {
				t.Errorf("delete: %v", err)
			}
		}()
		wg.Wait()
		got, _ := e.db.Recording(ctx, rec.PublicID)
		if got.Status != store.RecordingDeleted || got.FailureCode != "" {
			t.Fatalf("round %d: recording = %+v", round, got)
		}
	}
}

func TestAuditDetailsCarryOnlyIDsCountsAndCodes(t *testing.T) {
	e := newEnv(t, nil)
	rec := recordSample(t, e)
	ctx := context.Background()
	if _, err := e.svc.Events(ctx, rec.PublicID, recording.Query{}, ""); err != nil {
		t.Fatal(err)
	}
	export, err := e.svc.PrepareExport(ctx, rec.PublicID, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = export.WriteTo(ctx, io.Discard)
	if _, err := e.svc.Delete(ctx, rec.PublicID, ""); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"recordingId": true, "terminalId": true, "events": true, "chunks": true,
		"bytes": true, "code": true, "count": true}
	events, _ := e.db.AuditEvents(ctx)
	seen := 0
	for _, event := range events {
		if !strings.HasPrefix(event.Type, "recording.") {
			continue
		}
		seen++
		for key := range event.Details {
			if !allowed[key] {
				t.Errorf("%s has detail %q", event.Type, key)
			}
		}
	}
	if seen < 5 {
		t.Fatalf("only %d recording audits", seen)
	}
}

// An export that has begun streams consistent snapshots: deleting the
// recording between reads stops it with ErrDeleted, having written at most
// a prefix of whole lines, and nothing is marked corrupt.
func TestDeletionDuringExportStopsItCleanly(t *testing.T) {
	e := newEnv(t, func(c *recording.Config) { c.ChunkEvents = 1 })
	info, p := e.create(session.CreateRequest{})
	sub := subscribe(t, e.manager, info.PublicID)
	for i := 0; i < 80; i++ {
		p.Emit(fmt.Sprintf("line %d\r\n", i))
		nextOutput(t, sub)
	}
	e.terminate(info.PublicID)
	rec := e.waitStatus(info.PublicID, store.RecordingComplete)
	ctx := context.Background()
	export, err := e.svc.PrepareExport(ctx, rec.PublicID, "")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	e.faulty.setAfterChunks(func() {
		once.Do(func() {
			if _, err := e.svc.Delete(ctx, rec.PublicID, ""); err != nil {
				t.Errorf("delete: %v", err)
			}
		})
	})
	var out bytes.Buffer
	if err := export.WriteTo(ctx, &out); !errors.Is(err, recording.ErrDeleted) {
		t.Fatalf("WriteTo = %v, want ErrDeleted", err)
	}
	if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("export ends mid-line: %q", out.String())
	}
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")[1:] {
		var event []any
		if err := json.Unmarshal([]byte(line), &event); err != nil || len(event) != 3 {
			t.Fatalf("partial line in export prefix: %q", line)
		}
	}
	got, _ := e.db.Recording(ctx, rec.PublicID)
	if got.Status != store.RecordingDeleted || got.FailureCode != "" {
		t.Fatalf("recording = %+v", got)
	}
}
