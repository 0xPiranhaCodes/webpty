package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func TestRecordingsOutliveTheirTerminalMetadata(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "kept")
	appendChunk(t, s, "kept", chunk(0, 1, 1, 0, 0, "a"))
	finish(t, s, "kept", completeEnd(recordingStart.Add(time.Second)))
	createRecording(t, s, "term", "gone")
	finish(t, s, "gone", completeEnd(recordingStart.Add(time.Second)))
	if err := s.WithTx(ctx, func(tx *store.Tx) error {
		_, _, err := tx.DeleteRecording(ctx, "gone", recordingStart.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().Exec(`DELETE FROM terminal_sessions WHERE public_id = 'term'`); err != nil {
		t.Fatal(err)
	}
	tomb, err := s.Recording(ctx, "gone")
	if err != nil || tomb.Status != store.RecordingDeleted || tomb.TerminalID != "term" {
		t.Fatalf("tombstone after terminal delete = %+v, %v", tomb, err)
	}
	kept, err := s.Recording(ctx, "kept")
	if err != nil || kept.Status != store.RecordingComplete || kept.TerminalID != "term" {
		t.Fatalf("recording after terminal delete = %+v, %v", kept, err)
	}
	if chunkRows(t, s, "kept") != 1 {
		t.Fatal("chunks lost with the terminal metadata")
	}
	listed, err := s.Recordings(ctx, store.RecordingFilter{TerminalID: "term", Limit: 10})
	if err != nil || len(listed) != 2 {
		t.Fatalf("filtered list = %d recordings, %v", len(listed), err)
	}
}

func TestRecordingsPageByCursorWithStableTieBreaks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "a")
	seedTerminal(t, s, "b")
	// Same start time throughout, so ordering relies on the tie-break.
	for _, id := range []string{"a1", "a2", "a3"} {
		createRecording(t, s, "a", id)
	}
	createRecording(t, s, "b", "b1")

	var seen []string
	before := ""
	for page := 0; page < 5; page++ {
		recs, err := s.Recordings(ctx, store.RecordingFilter{Limit: 3, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs {
			seen = append(seen, rec.PublicID)
		}
		if len(recs) < 3 {
			break
		}
		before = recs[len(recs)-1].PublicID
	}
	if got := strings.Join(seen, ","); got != "b1,a3,a2,a1" {
		t.Fatalf("pages = %s, want every recording once, newest first", got)
	}
	recs, err := s.Recordings(ctx, store.RecordingFilter{TerminalID: "a", Limit: 2, Before: "a3"})
	if err != nil || len(recs) != 2 || recs[0].PublicID != "a2" || recs[1].PublicID != "a1" {
		t.Fatalf("filtered page = %+v, %v", recs, err)
	}
	if _, err := s.Recordings(ctx, store.RecordingFilter{Limit: 2, Before: "missing"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown cursor err = %v, want ErrNotFound", err)
	}
}

func TestRecordingChunksPageByEventCount(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")
	appendChunk(t, s, "rec", chunk(0, 1, 3, 0, 100, "c0"))
	appendChunk(t, s, "rec", chunk(1, 4, 6, 100, 200, "c1"))
	appendChunk(t, s, "rec", chunk(2, 7, 9, 200, 300, "c2"))

	page, err := s.RecordingChunks(ctx, "rec", store.ChunkQuery{MaxEvents: 4, MaxChunks: 10})
	if err != nil || len(page.Chunks) != 2 || !page.More {
		t.Fatalf("4 events = %d chunks, more=%v, %v; want the 2 chunks holding them and more", len(page.Chunks), page.More, err)
	}
	page, err = s.RecordingChunks(ctx, "rec", store.ChunkQuery{MaxEvents: 3, MaxChunks: 10})
	if err != nil || len(page.Chunks) != 1 || !page.More {
		t.Fatalf("3 events = %d chunks, more=%v, %v", len(page.Chunks), page.More, err)
	}
	page, err = s.RecordingChunks(ctx, "rec", store.ChunkQuery{MaxEvents: 100, MaxChunks: 10})
	if err != nil || len(page.Chunks) != 3 || page.More {
		t.Fatalf("all = %d chunks, more=%v, %v", len(page.Chunks), page.More, err)
	}
	// Exactly MaxChunks chunks remaining is the last page, not a full one.
	page, err = s.RecordingChunks(ctx, "rec", store.ChunkQuery{MaxChunks: 3})
	if err != nil || len(page.Chunks) != 3 || page.More {
		t.Fatalf("exact chunk limit = %d chunks, more=%v, %v", len(page.Chunks), page.More, err)
	}
	page, err = s.RecordingChunks(ctx, "rec", store.ChunkQuery{AfterSeq: 3, MaxEvents: 1, MaxChunks: 10})
	if err != nil || len(page.Chunks) != 1 || page.Chunks[0].Index != 1 || !page.More {
		t.Fatalf("seek = %+v, %v", page, err)
	}
}

// Playback reads take a snapshot without the write lock, so they never wait
// behind, or block, a recording flush.
func TestRecordingChunksDoNotWaitForWriters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTerminal(t, s, "term")
	createRecording(t, s, "term", "rec")
	appendChunk(t, s, "rec", chunk(0, 1, 1, 0, 0, "a"))

	holding, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- s.WithTx(ctx, func(tx *store.Tx) error {
			if err := tx.SetAdminCredential(ctx, "hash", recordingStart); err != nil {
				return err
			}
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	page, err := s.RecordingChunks(readCtx, "rec", store.ChunkQuery{MaxChunks: 10})
	close(release)
	if err != nil || len(page.Chunks) != 1 {
		t.Fatalf("read during a write = %+v, %v", page, err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}
