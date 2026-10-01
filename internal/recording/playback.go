package recording

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// Page limits.
const (
	DefaultPageEvents = 500
	MaxPageEvents     = 5000
	DefaultListLimit  = 100
	MaxListLimit      = 500
	// pageChunks bounds the chunks read and decompressed per query, and
	// scanPageEvents the events per read of a full scan.
	pageChunks     = 64
	scanPageEvents = MaxPageEvents
)

// Query selects events: those after sequence AfterSeq whose offset is at
// least AfterMS, at most Limit of them. A zero Limit uses DefaultPageEvents.
type Query struct {
	AfterMS  int64
	AfterSeq int64
	Limit    int
}

// Cursor continues a query where a page ended.
type Cursor struct {
	AfterMS  int64
	AfterSeq int64
}

// Page is a validated run of events. Next is nil when no events remain.
type Page struct {
	Recording store.Recording
	Events    []Event
	Next      *Cursor
}

// Recording returns a recording's metadata, tombstones included.
func (s *Service) Recording(ctx context.Context, id string) (store.Recording, error) {
	rec, err := s.cfg.Store.Recording(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Recording{}, ErrNotFound
	}
	return rec, err
}

// ListQuery selects a page of recordings. A zero TerminalID matches every
// terminal; a zero Limit uses DefaultListLimit. Before continues from a
// previous page's NextCursor.
type ListQuery struct {
	TerminalID string
	Limit      int
	Before     string
}

// List is a page of recordings, newest first. NextCursor is empty on the
// last page.
type List struct {
	Recordings []store.Recording
	NextCursor string
}

// Recordings lists a page of recordings newest first.
func (s *Service) Recordings(ctx context.Context, q ListQuery) (List, error) {
	switch {
	case q.Limit == 0:
		q.Limit = DefaultListLimit
	case q.Limit < 0 || q.Limit > MaxListLimit:
		return List{}, fmt.Errorf("%w: limit must be 1..%d", ErrInvalidArgument, MaxListLimit)
	}
	recs, err := s.cfg.Store.Recordings(ctx, store.RecordingFilter{TerminalID: q.TerminalID, Limit: q.Limit + 1, Before: q.Before})
	if errors.Is(err, store.ErrNotFound) {
		return List{}, fmt.Errorf("%w: unknown cursor", ErrInvalidArgument)
	}
	if err != nil {
		return List{}, err
	}
	list := List{Recordings: recs}
	if len(recs) > q.Limit {
		list.Recordings = recs[:q.Limit]
		list.NextCursor = recs[q.Limit-1].PublicID
	}
	return list, nil
}

// Events returns validated events of a recording. Pages are deterministic:
// the same query always returns the same events. Corrupt data is never
// returned; it marks the recording incomplete and returns a *CorruptError.
func (s *Service) Events(ctx context.Context, id string, q Query, remoteAddr string) (Page, error) {
	switch {
	case q.Limit == 0:
		q.Limit = DefaultPageEvents
	case q.Limit < 0 || q.Limit > MaxPageEvents:
		return Page{}, fmt.Errorf("%w: limit must be 1..%d", ErrInvalidArgument, MaxPageEvents)
	}
	if q.AfterMS < 0 || q.AfterSeq < 0 {
		return Page{}, fmt.Errorf("%w: afterMs and afterSeq must not be negative", ErrInvalidArgument)
	}
	chunks, err := s.cfg.Store.RecordingChunks(ctx, id, store.ChunkQuery{
		AfterSeq: q.AfterSeq, AfterMS: q.AfterMS, MaxEvents: q.Limit, MaxChunks: pageChunks,
	})
	if errors.Is(err, store.ErrNotFound) {
		return Page{}, ErrNotFound
	}
	if err != nil {
		return Page{}, err
	}
	rec := chunks.Recording
	if rec.Status == store.RecordingDeleted {
		return Page{}, ErrDeleted
	}

	cur := newChunkCursor(rec, chunks.Previous)
	var events []Event
	examined, full := q.AfterSeq, false
	for _, c := range chunks.Chunks {
		decoded, err := cur.next(c)
		if err != nil {
			return Page{}, s.corrupted(ctx, rec, err)
		}
		for _, e := range decoded {
			if e.Seq <= q.AfterSeq || e.OffsetMS < q.AfterMS {
				continue
			}
			events = append(events, e)
			examined = e.Seq
			if len(events) == q.Limit {
				full = true
				break
			}
		}
		if full {
			break
		}
		examined = max(examined, c.LastSeq)
	}
	if rec.Status != store.RecordingActive && !full && !chunks.More {
		truncated := len(chunks.Chunks) > 0 && !cur.complete()
		missing := len(chunks.Chunks) == 0 && q.AfterSeq < rec.EventCount && q.AfterMS <= rec.DurationMS
		if truncated || missing {
			return Page{}, s.corrupted(ctx, rec, corrupt("count"))
		}
	}

	page := Page{Recording: rec, Events: events}
	if (full && examined < rec.EventCount) || (!full && chunks.More) {
		page.Next = &Cursor{AfterMS: q.AfterMS, AfterSeq: examined}
	}
	details := map[string]string{"recordingId": rec.PublicID, "terminalId": rec.TerminalID,
		"events": strconv.Itoa(len(events))}
	if err := s.auditNow(ctx, "recording.playback", remoteAddr, details); err != nil {
		return Page{}, err
	}
	return page, nil
}

// corrupted marks rec incomplete when err is a *CorruptError, and returns err.
// The mark and its audit outlive the request, so a client that disconnects
// cannot leave a corrupt recording marked complete.
func (s *Service) corrupted(reqCtx context.Context, rec store.Recording, err error) error {
	var cerr *CorruptError
	if !errors.As(err, &cerr) {
		return err
	}
	s.cfg.Logger.Error("recording failed validation", "recordingId", rec.PublicID, "check", cerr.Code)
	now := s.cfg.Now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), s.cfg.StoreTimeout)
	defer cancel()
	markErr := s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		changed, err := tx.MarkRecordingIncomplete(ctx, rec.PublicID, CodeCorrupt, now, now.Add(s.cfg.Retention))
		if err != nil || !changed {
			return err
		}
		return s.audit(ctx, tx, "recording.incomplete", "", map[string]string{
			"recordingId": rec.PublicID, "terminalId": rec.TerminalID, "code": CodeCorrupt,
		})
	})
	if markErr != nil {
		s.cfg.Logger.Error("mark recording corrupt", "recordingId", rec.PublicID, "error", markErr)
	}
	return err
}

// scan validates every chunk of an ended recording in order, calling fn for
// each event. Each read is a consistent snapshot; a recording deleted during
// the scan returns ErrDeleted.
func (s *Service) scan(ctx context.Context, id string, fn func(Event) error) (store.Recording, error) {
	var first store.Recording
	var cur chunkCursor
	for started := false; ; started = true {
		page, err := s.cfg.Store.RecordingChunks(ctx, id, store.ChunkQuery{
			AfterSeq: cur.seq, MaxEvents: scanPageEvents, MaxChunks: pageChunks,
		})
		if errors.Is(err, store.ErrNotFound) {
			return store.Recording{}, ErrNotFound
		}
		if err != nil {
			return store.Recording{}, err
		}
		rec := page.Recording
		switch {
		case rec.Status == store.RecordingDeleted:
			return rec, ErrDeleted
		case rec.Status == store.RecordingActive:
			return rec, ErrActive
		case !started:
			first = rec
			cur = newChunkCursor(rec, page.Previous)
		case rec.EventCount != first.EventCount || rec.ChunkCount != first.ChunkCount:
			return rec, s.corrupted(ctx, rec, corrupt("count"))
		}
		for _, c := range page.Chunks {
			events, err := cur.next(c)
			if err != nil {
				return rec, s.corrupted(ctx, rec, err)
			}
			for _, e := range events {
				if err := fn(e); err != nil {
					return rec, err
				}
			}
		}
		if !page.More {
			if !cur.complete() {
				return rec, s.corrupted(ctx, rec, corrupt("count"))
			}
			return first, nil
		}
	}
}

// Export is an ended recording that passed validation and can be written
// as an asciicast v2 file.
type Export struct {
	s   *Service
	rec store.Recording
}

// PrepareExport validates the whole recording before anything is written,
// so a corrupt recording fails before any output.
func (s *Service) PrepareExport(ctx context.Context, id, remoteAddr string) (*Export, error) {
	rec, err := s.Recording(ctx, id)
	switch {
	case err != nil:
		return nil, err
	case rec.Status == store.RecordingDeleted:
		return nil, ErrDeleted
	case rec.Status == store.RecordingActive:
		return nil, ErrActive
	}
	events := 0
	rec, err = s.scan(ctx, id, func(Event) error { events++; return nil })
	if err != nil {
		return nil, err
	}
	details := map[string]string{"recordingId": rec.PublicID, "terminalId": rec.TerminalID, "events": strconv.Itoa(events)}
	if err := s.auditNow(ctx, "recording.exported", remoteAddr, details); err != nil {
		return nil, err
	}
	return &Export{s: s, rec: rec}, nil
}

// Recording is the exported recording's metadata.
func (x *Export) Recording() store.Recording { return x.rec }

type castHeader struct {
	Version   int     `json:"version"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Timestamp int64   `json:"timestamp"`
	Duration  float64 `json:"duration"`
}

// WriteTo writes the recording as asciicast v2: a header with the initial
// size, then output ("o") and resize ("r") events with their timing. Input,
// lifecycle, and presence are never exported. Every chunk is validated
// again as it is written; if the recording is deleted or found corrupt
// meanwhile, WriteTo stops with an error after a partial, valid prefix.
func (x *Export) WriteTo(ctx context.Context, w io.Writer) error {
	out := bufio.NewWriter(w)
	header, err := json.Marshal(castHeader{Version: 2, Width: x.rec.Cols, Height: x.rec.Rows,
		Timestamp: x.rec.StartedAt.Unix(), Duration: seconds(x.rec.DurationMS)})
	if err != nil {
		return err
	}
	out.Write(header)
	out.WriteByte('\n')

	var carry []byte
	writeEvent := func(ms int64, code, text string) error {
		encoded, err := json.Marshal(text)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "[%s, %q, %s]\n", strconv.FormatFloat(seconds(ms), 'f', -1, 64), code, encoded)
		return nil
	}
	flushCarry := func(ms int64) error {
		if len(carry) == 0 {
			return nil
		}
		text := string(carry)
		carry = nil
		return writeEvent(ms, "o", text)
	}
	_, err = x.s.scan(ctx, x.rec.PublicID, func(e Event) error {
		switch e.Kind {
		case KindOutput:
			data := append(carry, e.Output...)
			complete, rest := splitIncompleteUTF8(data)
			carry = append([]byte(nil), rest...)
			if len(complete) == 0 {
				return nil
			}
			return writeEvent(e.OffsetMS, "o", string(complete))
		case KindResize:
			if err := flushCarry(e.OffsetMS); err != nil {
				return err
			}
			return writeEvent(e.OffsetMS, "r", fmt.Sprintf("%dx%d", e.Cols, e.Rows))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flushCarry(x.rec.DurationMS); err != nil {
		return err
	}
	return out.Flush()
}

func seconds(ms int64) float64 { return float64(ms) / 1000 }

// splitIncompleteUTF8 holds back a trailing, unfinished UTF-8 sequence so a
// character split across PTY reads is exported whole.
func splitIncompleteUTF8(b []byte) (complete, rest []byte) {
	for i := 1; i <= utf8.UTFMax-1 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c < utf8.RuneSelf {
			break
		}
		if utf8.RuneStart(c) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i], b[len(b)-i:]
			}
			break
		}
	}
	return b, nil
}

// Delete removes a recording's events and keeps its metadata as a
// tombstone. Deleting a deleted recording succeeds without change.
func (s *Service) Delete(ctx context.Context, id, remoteAddr string) (store.Recording, error) {
	var rec store.Recording
	err := s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		var changed bool
		var err error
		rec, changed, err = tx.DeleteRecording(ctx, id, s.cfg.Now())
		if err != nil || !changed {
			return err
		}
		return s.audit(ctx, tx, "recording.deleted", remoteAddr, countDetails(rec))
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Recording{}, ErrNotFound
	case errors.Is(err, store.ErrRecordingActive):
		return store.Recording{}, ErrActive
	}
	return rec, err
}

// RunRetention deletes every ended recording past its retention, in
// transactions of at most RetentionBatch recordings, and returns how many
// it deleted. Active recordings are never deleted.
func (s *Service) RunRetention(ctx context.Context) (int, error) {
	total := 0
	for {
		now := s.cfg.Now()
		var ids []string
		err := s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
			var err error
			if ids, err = tx.ExpiredRecordings(ctx, now, s.cfg.RetentionBatch); err != nil {
				return err
			}
			for _, id := range ids {
				rec, changed, err := tx.DeleteRecording(ctx, id, now)
				if err != nil {
					return err
				}
				if changed {
					if err := s.audit(ctx, tx, "recording.retention.deleted", "", countDetails(rec)); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		total += len(ids)
		if len(ids) < s.cfg.RetentionBatch {
			return total, nil
		}
	}
}
