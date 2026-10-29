package recording

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
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
	limit, err := pageLimit(q.Limit, DefaultListLimit, MaxListLimit)
	if err != nil {
		return List{}, err
	}
	q.Limit = limit
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

// pageLimit applies the default to a zero limit and rejects one outside 1..max.
func pageLimit(limit, fallback, max int) (int, error) {
	switch {
	case limit == 0:
		return fallback, nil
	case limit < 0 || limit > max:
		return 0, fmt.Errorf("%w: limit must be 1..%d", ErrInvalidArgument, max)
	}
	return limit, nil
}

// Events returns validated events of a recording. Pages are deterministic:
// the same query always returns the same events. Corrupt data is never
// returned; it marks the recording incomplete and returns a *CorruptError.
func (s *Service) Events(ctx context.Context, id string, q Query, remoteAddr string) (Page, error) {
	if err := q.normalize(); err != nil {
		return Page{}, err
	}
	chunks, err := s.chunkPage(ctx, id, store.ChunkQuery{
		AfterSeq: q.AfterSeq, AfterMS: q.AfterMS, MaxEvents: q.Limit, MaxChunks: pageChunks,
	})
	if err != nil {
		return Page{}, err
	}
	rec := chunks.Recording
	if rec.Status == store.RecordingDeleted {
		return Page{}, ErrDeleted
	}

	sel, err := s.validatedSelection(ctx, chunks, q)
	if err != nil {
		return Page{}, err
	}
	if err := s.auditNow(ctx, "recording.playback", remoteAddr, eventDetails(rec, len(sel.events))); err != nil {
		return Page{}, err
	}
	return Page{Recording: rec, Events: sel.events, Next: sel.next(rec, q, chunks.More)}, nil
}

// validatedSelection picks the query's events from chunks, marking the
// recording incomplete if they fail validation or fall short of its counts.
func (s *Service) validatedSelection(ctx context.Context, chunks store.RecordingChunkPage, q Query) (selection, error) {
	rec := chunks.Recording
	cur := newChunkCursor(rec, chunks.Previous)
	sel, err := selectEvents(&cur, chunks.Chunks, q)
	if err != nil {
		return selection{}, s.corrupted(ctx, rec, err)
	}
	if !sel.full && !chunks.More && endedShort(rec, chunks.Chunks, &cur, q) {
		return selection{}, s.corrupted(ctx, rec, corrupt("count"))
	}
	return sel, nil
}

// chunkPage reads a run of chunks, mapping a missing recording to ErrNotFound.
func (s *Service) chunkPage(ctx context.Context, id string, q store.ChunkQuery) (store.RecordingChunkPage, error) {
	page, err := s.cfg.Store.RecordingChunks(ctx, id, q)
	if errors.Is(err, store.ErrNotFound) {
		return store.RecordingChunkPage{}, ErrNotFound
	}
	return page, err
}

// eventDetails are the audit details of a read or export of n events.
func eventDetails(rec store.Recording, n int) map[string]string {
	return map[string]string{"recordingId": rec.PublicID, "terminalId": rec.TerminalID, "events": strconv.Itoa(n)}
}

// next is the cursor continuing after this selection, or nil when no events
// remain.
func (sel selection) next(rec store.Recording, q Query, more bool) *Cursor {
	if (sel.full && sel.examined < rec.EventCount) || (!sel.full && more) {
		return &Cursor{AfterMS: q.AfterMS, AfterSeq: sel.examined}
	}
	return nil
}

// normalize applies the default limit and rejects out-of-range values.
func (q *Query) normalize() error {
	limit, err := pageLimit(q.Limit, DefaultPageEvents, MaxPageEvents)
	if err != nil {
		return err
	}
	q.Limit = limit
	if q.AfterMS < 0 || q.AfterSeq < 0 {
		return fmt.Errorf("%w: afterMs and afterSeq must not be negative", ErrInvalidArgument)
	}
	return nil
}

// selection is the events a query picked from the chunks read. examined is
// the last sequence consumed, so the next page resumes after it; full is
// set when the limit was reached.
type selection struct {
	events   []Event
	examined int64
	full     bool
}

// selectEvents decodes chunks in order and keeps the events the query asks for.
func selectEvents(cur *chunkCursor, chunks []store.RecordingChunk, q Query) (selection, error) {
	sel := selection{examined: q.AfterSeq}
	for _, c := range chunks {
		decoded, err := cur.next(c)
		if err != nil {
			return selection{}, err
		}
		sel.take(decoded, q)
		if sel.full {
			break
		}
		sel.examined = max(sel.examined, c.LastSeq)
	}
	return sel, nil
}

// take appends the matching events of one chunk until the limit is reached.
func (sel *selection) take(decoded []Event, q Query) {
	for _, e := range decoded {
		if e.Seq <= q.AfterSeq || e.OffsetMS < q.AfterMS {
			continue
		}
		sel.events = append(sel.events, e)
		sel.examined = e.Seq
		if len(sel.events) == q.Limit {
			sel.full = true
			return
		}
	}
}

// endedShort reports whether an ended recording's last page falls short of
// its recorded counts: a truncated chunk run, or no chunks where some exist.
func endedShort(rec store.Recording, chunks []store.RecordingChunk, cur *chunkCursor, q Query) bool {
	if rec.Status == store.RecordingActive {
		return false
	}
	truncated := len(chunks) > 0 && !cur.complete()
	missing := len(chunks) == 0 && q.AfterSeq < rec.EventCount && q.AfterMS <= rec.DurationMS
	return truncated || missing
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
	if markErr := s.markIncomplete(reqCtx, rec); markErr != nil {
		s.cfg.Logger.Error("mark recording corrupt", "recordingId", rec.PublicID, "error", markErr)
	}
	return err
}

func (s *Service) markIncomplete(reqCtx context.Context, rec store.Recording) error {
	now := s.cfg.Now()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), s.cfg.StoreTimeout)
	defer cancel()
	return s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		changed, err := tx.MarkRecordingIncomplete(ctx, rec.PublicID, CodeCorrupt, now, now.Add(s.cfg.Retention))
		if err != nil || !changed {
			return err
		}
		return s.audit(ctx, tx, "recording.incomplete", "", map[string]string{
			"recordingId": rec.PublicID, "terminalId": rec.TerminalID, "code": CodeCorrupt,
		})
	})
}

// scan validates every chunk of an ended recording in order, calling fn for
// each event. Each read is a consistent snapshot; a recording deleted during
// the scan returns ErrDeleted.
func (s *Service) scan(ctx context.Context, id string, fn func(Event) error) (store.Recording, error) {
	var first store.Recording
	var cur chunkCursor
	for started := false; ; started = true {
		page, err := s.chunkPage(ctx, id, store.ChunkQuery{AfterSeq: cur.seq, MaxEvents: scanPageEvents, MaxChunks: pageChunks})
		if err != nil {
			return store.Recording{}, err
		}
		rec := page.Recording
		if err := s.checkScanPage(ctx, rec, first, started); err != nil {
			return rec, err
		}
		if !started {
			first = rec
			cur = newChunkCursor(rec, page.Previous)
		}
		if err := s.scanChunks(ctx, &cur, rec, page.Chunks, fn); err != nil {
			return rec, err
		}
		if !page.More {
			return s.endScan(ctx, &cur, first, rec)
		}
	}
}

// endScan accepts a scan that reached the last chunk only if every chunk
// the recording counts was seen.
func (s *Service) endScan(ctx context.Context, cur *chunkCursor, first, rec store.Recording) (store.Recording, error) {
	if !cur.complete() {
		return rec, s.corrupted(ctx, rec, corrupt("count"))
	}
	return first, nil
}

// checkScanPage rejects a page whose recording was deleted, is still active,
// or changed size since the scan began.
func (s *Service) checkScanPage(ctx context.Context, rec, first store.Recording, started bool) error {
	switch {
	case rec.Status == store.RecordingDeleted:
		return ErrDeleted
	case rec.Status == store.RecordingActive:
		return ErrActive
	case started && (rec.EventCount != first.EventCount || rec.ChunkCount != first.ChunkCount):
		return s.corrupted(ctx, rec, corrupt("count"))
	}
	return nil
}

// scanChunks validates chunks through cur and passes each event to fn.
func (s *Service) scanChunks(ctx context.Context, cur *chunkCursor, rec store.Recording, chunks []store.RecordingChunk, fn func(Event) error) error {
	for _, c := range chunks {
		events, err := cur.next(c)
		if err != nil {
			return s.corrupted(ctx, rec, err)
		}
		for _, e := range events {
			if err := fn(e); err != nil {
				return err
			}
		}
	}
	return nil
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
	if err := s.auditNow(ctx, "recording.exported", remoteAddr, eventDetails(rec, events)); err != nil {
		return nil, err
	}
	return &Export{s: s, rec: rec}, nil
}

// Recording is the exported recording's metadata.
func (x *Export) Recording() store.Recording { return x.rec }

// asciicast v2 version and event codes.
const (
	castVersion = 2
	castOutput  = "o"
	castResize  = "r"
)

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
	header, err := json.Marshal(castHeader{Version: castVersion, Width: x.rec.Cols, Height: x.rec.Rows,
		Timestamp: x.rec.StartedAt.Unix(), Duration: seconds(x.rec.DurationMS)})
	if err != nil {
		return err
	}
	out.Write(header)
	out.WriteByte('\n')

	cw := &castWriter{out: out}
	if _, err := x.s.scan(ctx, x.rec.PublicID, cw.add); err != nil {
		return err
	}
	if err := cw.flushCarry(x.rec.DurationMS); err != nil {
		return err
	}
	return out.Flush()
}

// castWriter writes asciicast event lines, holding back an unfinished UTF-8
// sequence (carry) until the rest of it arrives.
type castWriter struct {
	out   *bufio.Writer
	carry []byte
}

func (c *castWriter) add(e Event) error {
	switch e.Kind {
	case KindOutput:
		data := append(c.carry, e.Output...)
		complete, rest := splitIncompleteUTF8(data)
		c.carry = append([]byte(nil), rest...)
		if len(complete) == 0 {
			return nil
		}
		return c.event(e.OffsetMS, castOutput, string(complete))
	case KindResize:
		if err := c.flushCarry(e.OffsetMS); err != nil {
			return err
		}
		return c.event(e.OffsetMS, castResize, fmt.Sprintf("%dx%d", e.Cols, e.Rows))
	}
	return nil
}

func (c *castWriter) event(ms int64, code, text string) error {
	encoded, err := json.Marshal(text)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "[%s, %q, %s]\n", strconv.FormatFloat(seconds(ms), 'f', -1, 64), code, encoded)
	return nil
}

func (c *castWriter) flushCarry(ms int64) error {
	if len(c.carry) == 0 {
		return nil
	}
	text := string(c.carry)
	c.carry = nil
	return c.event(ms, castOutput, text)
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
		ids, err := s.deleteExpired(ctx)
		if err != nil {
			return total, err
		}
		total += len(ids)
		if len(ids) < s.cfg.RetentionBatch {
			return total, nil
		}
	}
}

// deleteExpired deletes one batch of expired recordings in a transaction and
// returns their IDs.
func (s *Service) deleteExpired(ctx context.Context) ([]string, error) {
	now := s.cfg.Now()
	var ids []string
	err := s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		if ids, err = tx.ExpiredRecordings(ctx, now, s.cfg.RetentionBatch); err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.deleteForRetention(ctx, tx, id, now); err != nil {
				return err
			}
		}
		return nil
	})
	return ids, err
}

func (s *Service) deleteForRetention(ctx context.Context, tx *store.Tx, id string, now time.Time) error {
	rec, changed, err := tx.DeleteRecording(ctx, id, now)
	if err != nil || !changed {
		return err
	}
	return s.audit(ctx, tx, "recording.retention.deleted", "", countDetails(rec))
}
