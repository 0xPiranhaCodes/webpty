package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// RecordingStatus is the lifecycle state of a terminal recording.
type RecordingStatus string

const (
	RecordingActive     RecordingStatus = "recording"
	RecordingComplete   RecordingStatus = "complete"
	RecordingIncomplete RecordingStatus = "incomplete"
	RecordingDeleted    RecordingStatus = "deleted"
)

var (
	// ErrRecordingNotActive reports a write to a recording that has ended
	// or been deleted.
	ErrRecordingNotActive = errors.New("store: recording is not active")
	// ErrRecordingActive reports an operation that requires an ended recording.
	ErrRecordingActive = errors.New("store: recording is active")
	// ErrChunkOutOfOrder reports a chunk that does not directly follow the
	// recording's last chunk in index, sequence, or offset.
	ErrChunkOutOfOrder = errors.New("store: recording chunk out of order")
)

// Recording is the metadata of one terminal recording. Its events live in
// chunks; a deleted recording keeps its metadata as a tombstone. Both
// outlive the terminal's own metadata.
type Recording struct {
	PublicID          string
	TerminalID        string
	Status            RecordingStatus
	FormatVersion     int
	Codec             string
	StartedAt         time.Time
	EndedAt           time.Time
	DurationMS        int64
	Rows, Cols        int
	EventCount        int64
	ChunkCount        int64
	CompressedBytes   int64
	UncompressedBytes int64
	FailureCode       string
	RetainUntil       time.Time
	DeletedAt         time.Time
	UpdatedAt         time.Time
}

// RecordingChunk is one independently compressed run of consecutive events.
type RecordingChunk struct {
	Index             int64
	FirstSeq, LastSeq int64
	FirstOffsetMS     int64
	LastOffsetMS      int64
	EventCount        int64
	UncompressedBytes int64
	Checksum          []byte
	Codec             string
	Data              []byte
}

// RecordingEnd is how a recording finished.
type RecordingEnd struct {
	Status      RecordingStatus
	FailureCode string
	EndedAt     time.Time
	DurationMS  int64
	RetainUntil time.Time
}

// RecordingFilter selects recordings to list. A zero TerminalID matches all.
// Before, if set, is the public ID of a recording; only recordings listed
// after it are returned.
type RecordingFilter struct {
	TerminalID string
	Limit      int
	Before     string
}

// RecordingChunkPage is a consistent snapshot of a recording and a run of
// its chunks. Previous, when set, is the metadata (without Data) of the chunk
// just before the first returned chunk. More reports that chunks follow the
// last returned one.
type RecordingChunkPage struct {
	Recording Recording
	Previous  *RecordingChunk
	Chunks    []RecordingChunk
	More      bool
}

// ChunkQuery selects chunks starting with the first that holds an event
// after AfterSeq at or after AfterMS. It returns chunks until they hold at
// least MaxEvents events after AfterSeq, or MaxChunks chunks; a zero bound
// is unlimited.
type ChunkQuery struct {
	AfterSeq, AfterMS    int64
	MaxEvents, MaxChunks int
}

const recordingColumns = `r.public_id, r.terminal_public_id, r.status, r.format_version, r.codec, r.started_at, r.ended_at,
	r.duration_ms, r.rows, r.cols, r.event_count, r.chunk_count, r.compressed_bytes, r.uncompressed_bytes,
	r.failure_code, r.retain_until, r.deleted_at, r.updated_at`

const recordingFrom = ` FROM recordings r`

const chunkMetaColumns = `chunk_index, first_seq, last_seq, first_offset_ms, last_offset_ms, event_count,
	uncompressed_bytes, checksum, codec`

type rowsQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// CreateRecording inserts an active recording of rec.TerminalID, or returns
// ErrNotFound if that terminal does not exist.
func (t *Tx) CreateRecording(ctx context.Context, rec Recording) error {
	result, err := t.tx.ExecContext(ctx, `INSERT INTO recordings (public_id, terminal_session_id, terminal_public_id,
			status, format_version, codec, started_at, rows, cols, updated_at)
		SELECT ?, id, public_id, 'recording', ?, ?, ?, ?, ?, ? FROM terminal_sessions WHERE public_id = ?`,
		rec.PublicID, rec.FormatVersion, rec.Codec, toDB(rec.StartedAt), rec.Rows, rec.Cols, toDB(rec.StartedAt), rec.TerminalID)
	if err != nil {
		return fmt.Errorf("create recording: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateFailedRecording inserts metadata for a recording that failed before
// it could record anything: incomplete with rec.FailureCode, ended when it
// started, and kept until rec.RetainUntil. It returns ErrNotFound if the
// terminal does not exist.
func (t *Tx) CreateFailedRecording(ctx context.Context, rec Recording) error {
	if rec.FailureCode == "" {
		return errors.New("create failed recording: failure code required")
	}
	result, err := t.tx.ExecContext(ctx, `INSERT INTO recordings (public_id, terminal_session_id, terminal_public_id,
			status, format_version, codec, started_at, ended_at, rows, cols, failure_code, retain_until, updated_at)
		SELECT ?, id, public_id, 'incomplete', ?, ?, ?, ?, ?, ?, ?, ?, ? FROM terminal_sessions WHERE public_id = ?`,
		rec.PublicID, rec.FormatVersion, rec.Codec, toDB(rec.StartedAt), toDB(rec.StartedAt), rec.Rows, rec.Cols,
		rec.FailureCode, toDB(rec.RetainUntil), toDB(rec.StartedAt), rec.TerminalID)
	if err != nil {
		return fmt.Errorf("create failed recording: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendRecordingChunk stores c and advances the recording's counters in
// one transaction. c must directly follow the previous chunk.
func (s *Store) AppendRecordingChunk(ctx context.Context, publicID string, c RecordingChunk, now time.Time) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		id, err := tx.advanceRecording(ctx, publicID, c, now)
		if err != nil {
			return err
		}
		return tx.insertChunk(ctx, id, c)
	})
}

// advanceRecording bumps the counters for c, and returns the recording's row
// ID. It matches only when c directly follows the previous chunk.
func (t *Tx) advanceRecording(ctx context.Context, publicID string, c RecordingChunk, now time.Time) (int64, error) {
	var id int64
	err := t.tx.QueryRowContext(ctx, `UPDATE recordings SET chunk_count = chunk_count + 1,
			event_count = event_count + ?, compressed_bytes = compressed_bytes + ?,
			uncompressed_bytes = uncompressed_bytes + ?, duration_ms = ?, updated_at = ?
		WHERE public_id = ? AND status = 'recording' AND chunk_count = ? AND event_count + 1 = ? AND duration_ms <= ?
		RETURNING id`,
		c.EventCount, len(c.Data), c.UncompressedBytes, c.LastOffsetMS, toDB(now),
		publicID, c.Index, c.FirstSeq, c.FirstOffsetMS).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, t.appendRejected(ctx, publicID)
	}
	if err != nil {
		return 0, fmt.Errorf("append recording chunk: %w", err)
	}
	return id, nil
}

func (t *Tx) insertChunk(ctx context.Context, recordingID int64, c RecordingChunk) error {
	if _, err := t.tx.ExecContext(ctx, `INSERT INTO recording_chunks (recording_id, `+chunkMetaColumns+`, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		recordingID, c.Index, c.FirstSeq, c.LastSeq, c.FirstOffsetMS, c.LastOffsetMS, c.EventCount,
		c.UncompressedBytes, c.Checksum, c.Codec, c.Data); err != nil {
		return fmt.Errorf("append recording chunk: %w", err)
	}
	return nil
}

func (t *Tx) appendRejected(ctx context.Context, publicID string) error {
	var status string
	err := t.tx.QueryRowContext(ctx, `SELECT status FROM recordings WHERE public_id = ?`, publicID).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("append recording chunk: %w", err)
	case RecordingStatus(status) != RecordingActive:
		return ErrRecordingNotActive
	default:
		return ErrChunkOutOfOrder
	}
}

// FinishRecording ends an active recording as complete or incomplete, or
// returns ErrRecordingNotActive.
func (t *Tx) FinishRecording(ctx context.Context, publicID string, end RecordingEnd) (Recording, error) {
	if end.Status != RecordingComplete && end.Status != RecordingIncomplete {
		return Recording{}, fmt.Errorf("finish recording: invalid status %q", end.Status)
	}
	result, err := t.tx.ExecContext(ctx, `UPDATE recordings SET status = ?, failure_code = ?, ended_at = ?,
			retain_until = ?, updated_at = ?, duration_ms = MAX(duration_ms, ?)
		WHERE public_id = ? AND status = 'recording'`,
		string(end.Status), end.FailureCode, toDB(end.EndedAt), toDB(end.RetainUntil), toDB(end.EndedAt),
		end.DurationMS, publicID)
	if err != nil {
		return Recording{}, fmt.Errorf("finish recording: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		if _, err := t.Recording(ctx, publicID); err != nil {
			return Recording{}, err
		}
		return Recording{}, ErrRecordingNotActive
	}
	return t.Recording(ctx, publicID)
}

// MarkRecordingIncomplete marks an active or complete recording incomplete
// with code. An active recording also ends at now. changed is false for
// recordings that were already incomplete or deleted.
func (t *Tx) MarkRecordingIncomplete(ctx context.Context, publicID, code string, now, retainUntil time.Time) (bool, error) {
	result, err := t.tx.ExecContext(ctx, `UPDATE recordings SET status = 'incomplete', failure_code = ?,
			ended_at = COALESCE(ended_at, ?), retain_until = COALESCE(retain_until, ?), updated_at = ?
		WHERE public_id = ? AND status IN ('recording', 'complete')`,
		code, toDB(now), toDB(retainUntil), toDB(now), publicID)
	if err != nil {
		return false, fmt.Errorf("mark recording incomplete: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark recording incomplete: %w", err)
	}
	return affected > 0, nil
}

// DeleteRecording removes an ended recording's chunks and tombstones its
// metadata. changed is false if it was already deleted. Active recordings
// return ErrRecordingActive.
func (t *Tx) DeleteRecording(ctx context.Context, publicID string, now time.Time) (rec Recording, changed bool, err error) {
	rec, err = t.Recording(ctx, publicID)
	if err != nil {
		return Recording{}, false, err
	}
	switch rec.Status {
	case RecordingDeleted:
		return rec, false, nil
	case RecordingActive:
		return rec, false, ErrRecordingActive
	}
	if err := t.tombstoneRecording(ctx, publicID, now); err != nil {
		return Recording{}, false, err
	}
	rec, err = t.Recording(ctx, publicID)
	return rec, err == nil, err
}

// tombstoneRecording marks an ended recording deleted and drops its chunks.
func (t *Tx) tombstoneRecording(ctx context.Context, publicID string, now time.Time) error {
	var id int64
	err := t.tx.QueryRowContext(ctx, `UPDATE recordings SET status = 'deleted', deleted_at = ?, updated_at = ?
		WHERE public_id = ? AND status IN ('complete', 'incomplete') RETURNING id`,
		toDB(now), toDB(now), publicID).Scan(&id)
	if err != nil {
		return fmt.Errorf("delete recording: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM recording_chunks WHERE recording_id = ?`, id); err != nil {
		return fmt.Errorf("delete recording chunks: %w", err)
	}
	return nil
}

// ExpiredRecordings returns up to limit ended recordings whose retention
// ended at or before now, oldest first.
func (t *Tx) ExpiredRecordings(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return queryIDs(ctx, t.tx, `SELECT public_id FROM recordings
		WHERE status IN ('complete', 'incomplete') AND retain_until <= ? ORDER BY retain_until, id LIMIT ?`,
		toDB(now), limit)
}

// FailInterruptedRecordings marks recordings left active by a previous
// process as incomplete and returns their IDs.
func (t *Tx) FailInterruptedRecordings(ctx context.Context, now, retainUntil time.Time) ([]string, error) {
	ids, err := queryIDs(ctx, t.tx, `UPDATE recordings SET status = 'incomplete', failure_code = 'interrupted',
		ended_at = ?1, retain_until = ?2, updated_at = ?1 WHERE status = 'recording' RETURNING public_id`,
		toDB(now), toDB(retainUntil))
	sort.Strings(ids)
	return ids, err
}

func queryIDs(ctx context.Context, q rowsQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query recordings: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("query recordings: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Recording returns the recording with publicID, or ErrNotFound.
func (s *Store) Recording(ctx context.Context, publicID string) (Recording, error) {
	return recordingBy(ctx, s.db, publicID)
}

// Recording returns the recording with publicID, or ErrNotFound.
func (t *Tx) Recording(ctx context.Context, publicID string) (Recording, error) {
	return recordingBy(ctx, t.tx, publicID)
}

func recordingBy(ctx context.Context, q queryer, publicID string) (Recording, error) {
	rec, err := scanRecording(q.QueryRowContext(ctx, `SELECT `+recordingColumns+recordingFrom+` WHERE r.public_id = ?`, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, ErrNotFound
	}
	return rec, err
}

// Recordings lists recordings newest first, tombstones included. It returns
// ErrNotFound if filter.Before names no recording.
func (s *Store) Recordings(ctx context.Context, filter RecordingFilter) ([]Recording, error) {
	query, args, err := s.recordingsQuery(ctx, filter)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY r.started_at DESC, r.id DESC LIMIT ?`, append(args, filter.Limit)...)
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	defer rows.Close()
	var recs []Recording
	for rows.Next() {
		rec, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return recs, rows.Err()
}

// recordingsQuery builds the filtered SELECT, without ordering or limit.
func (s *Store) recordingsQuery(ctx context.Context, filter RecordingFilter) (string, []any, error) {
	where, args := []string{}, []any{}
	if filter.TerminalID != "" {
		where, args = append(where, `r.terminal_public_id = ?`), append(args, filter.TerminalID)
	}
	if filter.Before != "" {
		startedAt, id, err := s.recordingPosition(ctx, filter.Before)
		if err != nil {
			return "", nil, err
		}
		where, args = append(where, `(r.started_at, r.id) < (?, ?)`), append(args, startedAt, id)
	}
	query := `SELECT ` + recordingColumns + recordingFrom
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	return query, args, nil
}

// recordingPosition returns the sort key a Before cursor continues from.
func (s *Store) recordingPosition(ctx context.Context, publicID string) (startedAt, id int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT started_at, id FROM recordings WHERE public_id = ?`, publicID).Scan(&startedAt, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("list recordings: %w", err)
	}
	return startedAt, id, nil
}

// RecordingChunks reads, in one snapshot, the recording and a run of its
// chunks selected by q. The read-only transaction is deferred, so it neither
// takes nor waits for the write lock.
func (s *Store) RecordingChunks(ctx context.Context, publicID string, q ChunkQuery) (RecordingChunkPage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RecordingChunkPage{}, fmt.Errorf("begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id int64
	rec, err := scanRecordingWith(tx.QueryRowContext(ctx,
		`SELECT r.id, `+recordingColumns+recordingFrom+` WHERE r.public_id = ?`, publicID), &id)
	if errors.Is(err, sql.ErrNoRows) {
		return RecordingChunkPage{}, ErrNotFound
	}
	if err != nil {
		return RecordingChunkPage{}, err
	}
	return readChunkPage(ctx, tx, RecordingChunkPage{Recording: rec}, id, q)
}

// readChunkPage fills page with the chunks q selects from recording id.
func readChunkPage(ctx context.Context, tx *sql.Tx, page RecordingChunkPage, id int64, q ChunkQuery) (RecordingChunkPage, error) {
	start, found, err := firstChunkIndex(ctx, tx, id, q)
	if err != nil {
		return RecordingChunkPage{}, err
	}
	if !found {
		return page, nil
	}
	if page.Previous, err = previousChunk(ctx, tx, id, start); err != nil {
		return RecordingChunkPage{}, err
	}
	end, more, err := chunkPageEnd(ctx, tx, id, start, q)
	if err != nil {
		return RecordingChunkPage{}, err
	}
	page.More = more
	if page.Chunks, err = readChunks(ctx, tx, id, start, end); err != nil {
		return RecordingChunkPage{}, err
	}
	return page, nil
}

// firstChunkIndex finds the first chunk holding an event the query selects.
func firstChunkIndex(ctx context.Context, tx *sql.Tx, id int64, q ChunkQuery) (start int64, found bool, err error) {
	var first sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(chunk_index) FROM recording_chunks
		WHERE recording_id = ? AND last_seq > ? AND last_offset_ms >= ?`, id, q.AfterSeq, q.AfterMS).Scan(&first); err != nil {
		return 0, false, fmt.Errorf("seek recording: %w", err)
	}
	return first.Int64, first.Valid, nil
}

// previousChunk returns the metadata of the chunk before start, or nil if
// start is the first.
func previousChunk(ctx context.Context, tx *sql.Tx, id, start int64) (*RecordingChunk, error) {
	if start == 0 {
		return nil, nil
	}
	var prev RecordingChunk
	err := scanChunk(tx.QueryRowContext(ctx, `SELECT `+chunkMetaColumns+` FROM recording_chunks
		WHERE recording_id = ? AND chunk_index = ?`, id, start-1), &prev)
	switch {
	case err == nil:
		return &prev, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	default:
		return nil, err
	}
}

// readChunks reads chunks start..end with their data.
func readChunks(ctx context.Context, tx *sql.Tx, id, start, end int64) ([]RecordingChunk, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+chunkMetaColumns+`, data FROM recording_chunks
		WHERE recording_id = ? AND chunk_index >= ? AND chunk_index <= ? ORDER BY chunk_index`, id, start, end)
	if err != nil {
		return nil, fmt.Errorf("read recording chunks: %w", err)
	}
	defer rows.Close()
	var chunks []RecordingChunk
	for rows.Next() {
		var c RecordingChunk
		if err := scanChunk(rows, &c, &c.Data); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// satisfied reports whether chunks and events already meet a bound; zero
// bounds are unlimited.
func (q ChunkQuery) satisfied(chunks, events int64) bool {
	return (q.MaxChunks > 0 && chunks >= int64(q.MaxChunks)) || (q.MaxEvents > 0 && events >= int64(q.MaxEvents))
}

// chunkPageEnd finds, from chunk metadata alone, the last chunk index of the
// page starting at start, and whether any chunk follows it.
func chunkPageEnd(ctx context.Context, tx *sql.Tx, id, start int64, q ChunkQuery) (end int64, more bool, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT chunk_index, last_seq - MAX(first_seq - 1, ?) FROM recording_chunks
		WHERE recording_id = ? AND chunk_index >= ? ORDER BY chunk_index`, q.AfterSeq, id, start)
	if err != nil {
		return 0, false, fmt.Errorf("page recording chunks: %w", err)
	}
	defer rows.Close()
	end = start - 1
	var chunks, events int64
	for rows.Next() {
		if q.satisfied(chunks, events) {
			return end, true, nil
		}
		var count int64
		if err := rows.Scan(&end, &count); err != nil {
			return 0, false, fmt.Errorf("page recording chunks: %w", err)
		}
		chunks++
		events += count
	}
	return end, false, rows.Err()
}

// Backup writes a transactionally consistent copy of the database to path,
// which must not exist. Recording chunks and their counters, and deletion
// tombstones, are always copied together.
func (s *Store) Backup(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: %s already exists", path)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

func scanChunk(row rowScanner, c *RecordingChunk, extra ...any) error {
	dest := append([]any{&c.Index, &c.FirstSeq, &c.LastSeq, &c.FirstOffsetMS, &c.LastOffsetMS, &c.EventCount,
		&c.UncompressedBytes, &c.Checksum, &c.Codec}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return fmt.Errorf("scan recording chunk: %w", err)
	}
	return nil
}

func scanRecording(row rowScanner) (Recording, error) { return scanRecordingWith(row) }

// recordingRow holds a recordings row's raw column values.
type recordingRow struct {
	rec                             Recording
	status                          string
	startedAt, updatedAt            int64
	endedAt, retainUntil, deletedAt sql.NullInt64
}

// dest lists the scan targets in recordingColumns order, after leading.
func (r *recordingRow) dest(leading []any) []any {
	rec := &r.rec
	return append(leading, &rec.PublicID, &rec.TerminalID, &r.status, &rec.FormatVersion, &rec.Codec, &r.startedAt,
		&r.endedAt, &rec.DurationMS, &rec.Rows, &rec.Cols, &rec.EventCount, &rec.ChunkCount, &rec.CompressedBytes,
		&rec.UncompressedBytes, &rec.FailureCode, &r.retainUntil, &r.deletedAt, &r.updatedAt)
}

func (r *recordingRow) recording() Recording {
	rec := r.rec
	rec.Status = RecordingStatus(r.status)
	rec.StartedAt = fromDB(r.startedAt)
	rec.UpdatedAt = fromDB(r.updatedAt)
	rec.EndedAt = fromNullDB(r.endedAt)
	rec.RetainUntil = fromNullDB(r.retainUntil)
	rec.DeletedAt = fromNullDB(r.deletedAt)
	return rec
}

func scanRecordingWith(row rowScanner, leading ...any) (Recording, error) {
	var r recordingRow
	if err := row.Scan(r.dest(leading)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Recording{}, err
		}
		return Recording{}, fmt.Errorf("scan recording: %w", err)
	}
	return r.recording(), nil
}
