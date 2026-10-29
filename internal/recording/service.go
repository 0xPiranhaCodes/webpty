// Package recording records terminal sessions without ever blocking them,
// and serves validated playback and export of the recordings.
//
// Only PTY output, terminal size, lifecycle, and presence changes are
// recorded; terminal input never is. Events are stored as versioned JSON
// lines in independently gzip-compressed, checksummed chunks.
package recording

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// Failure codes. They are safe to show to administrators and to audit.
const (
	CodeQueueOverflow   = "queue_overflow"
	CodeStorage         = "storage_error"
	CodeEncoding        = "encoding_error"
	CodeSizeLimit       = "size_limit"
	CodeShutdownTimeout = "shutdown_timeout"
	CodeInterrupted     = "interrupted"
	CodeCorrupt         = "corrupt"
)

var (
	ErrNotFound        = errors.New("recording: not found")
	ErrDeleted         = errors.New("recording: deleted")
	ErrActive          = errors.New("recording: still recording")
	ErrCorrupt         = errors.New("recording: corrupt")
	ErrInvalidArgument = errors.New("recording: invalid argument")
)

// CorruptError reports stored data that failed validation. Code names the
// failed check. It matches ErrCorrupt.
type CorruptError struct{ Code string }

func (e *CorruptError) Error() string        { return "recording: corrupt: " + e.Code }
func (e *CorruptError) Is(target error) bool { return target == ErrCorrupt }

func corrupt(code string) error { return &CorruptError{Code: code} }

// Timer is a stoppable pending callback.
type Timer interface {
	Stop() bool
}

// Store persists recordings. *store.Store implements it.
type Store interface {
	WithTx(context.Context, func(*store.Tx) error) error
	AppendRecordingChunk(ctx context.Context, id string, chunk store.RecordingChunk, now time.Time) error
	Recording(ctx context.Context, id string) (store.Recording, error)
	Recordings(ctx context.Context, filter store.RecordingFilter) ([]store.Recording, error)
	RecordingChunks(ctx context.Context, id string, q store.ChunkQuery) (store.RecordingChunkPage, error)
}

// Config tunes a Service. Zero fields receive defaults.
type Config struct {
	Store Store
	// Disabled turns recording off; playback of existing recordings works.
	Disabled bool
	// QueueBytes bounds the events one recording may have waiting for
	// storage. Overflowing it ends the recording as incomplete.
	QueueBytes int
	// ChunkBytes and ChunkEvents bound one chunk's uncompressed size and
	// event count; FlushInterval bounds how long a partial chunk waits.
	ChunkBytes    int
	ChunkEvents   int
	FlushInterval time.Duration
	// Retention is how long ended recordings are kept.
	Retention time.Duration
	// MaxRecordingBytes bounds one recording's uncompressed size.
	MaxRecordingBytes int64
	// ShutdownTimeout bounds how long Close waits for recordings to flush.
	ShutdownTimeout time.Duration
	// RetentionBatch bounds the recordings deleted per transaction.
	RetentionBatch int
	// StoreTimeout bounds each storage call made by a recorder.
	StoreTimeout time.Duration
	// StartTimeout bounds how long creating a recording may delay the
	// session's start. A recording that cannot start in time is reported
	// as incomplete; the session runs unrecorded.
	StartTimeout time.Duration

	Now       func() time.Time
	AfterFunc func(time.Duration, func()) Timer
	Logger    *slog.Logger
}

// Status reports a recording that stopped early.
type Status struct {
	RecordingID string
	TerminalID  string
	State       store.RecordingStatus
	Code        string
}

// abortGrace is how long Close waits for recorders after cancelling them.
const abortGrace = time.Second

// Service records sessions and serves recordings. It implements
// session.Recorders and collab.Observer.
type Service struct {
	cfg    Config
	encode func(Event) ([]byte, error)
	// ctx is cancelled when shutdown times out, aborting storage calls.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	active   map[string]*recorder
	failures map[string]Status
	watchers map[string]map[chan Status]struct{}
	wg       sync.WaitGroup
}

var (
	_ session.Recorders = (*Service)(nil)
	_ collab.Observer   = (*Service)(nil)
)

// Defaults for zero Config fields, and the bounds validate enforces.
const (
	defaultQueueBytes        = 1 << 20
	defaultChunkBytes        = 64 << 10
	defaultChunkEvents       = 1024
	defaultFlushInterval     = time.Second
	defaultRetention         = 30 * 24 * time.Hour
	defaultMaxRecordingBytes = 256 << 20
	defaultShutdownTimeout   = 5 * time.Second
	defaultRetentionBatch    = 100
	defaultStoreTimeout      = 10 * time.Second
	defaultStartTimeout      = time.Second

	minQueueBytes  = 16 << 10
	minChunkBytes  = 1 << 10
	maxChunkEvents = 1 << 16
)

// New validates cfg and marks recordings left active by a previous process
// as incomplete.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("recording: store is required")
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, encode: encodeEvent, active: map[string]*recorder{},
		failures: map[string]Status{}, watchers: map[string]map[chan Status]struct{}{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if err := s.failInterrupted(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (cfg *Config) applyDefaults() {
	setDefault(&cfg.QueueBytes, defaultQueueBytes)
	setDefault(&cfg.ChunkBytes, defaultChunkBytes)
	setDefault(&cfg.ChunkEvents, defaultChunkEvents)
	setDefault(&cfg.FlushInterval, defaultFlushInterval)
	setDefault(&cfg.Retention, defaultRetention)
	setDefault(&cfg.MaxRecordingBytes, defaultMaxRecordingBytes)
	setDefault(&cfg.ShutdownTimeout, defaultShutdownTimeout)
	setDefault(&cfg.RetentionBatch, defaultRetentionBatch)
	setDefault(&cfg.StoreTimeout, defaultStoreTimeout)
	setDefault(&cfg.StartTimeout, defaultStartTimeout)
	cfg.applyDeps()
}

func (cfg *Config) applyDeps() {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
}

func (cfg *Config) validate() error {
	switch {
	case cfg.QueueBytes < minQueueBytes:
		return errors.New("recording: queue must be at least 16KiB")
	case cfg.ChunkBytes < minChunkBytes || cfg.ChunkBytes > MaxChunkBytes:
		return fmt.Errorf("recording: chunk size must be 1KiB..%d bytes", MaxChunkBytes)
	case cfg.ChunkEvents < 1 || cfg.ChunkEvents > maxChunkEvents:
		return errors.New("recording: chunk event limit must be 1..65536")
	case cfg.MaxRecordingBytes < int64(cfg.ChunkBytes):
		return errors.New("recording: maximum recording size must be at least one chunk")
	case cfg.FlushInterval < 0 || cfg.Retention < 0 || cfg.ShutdownTimeout < 0 || cfg.RetentionBatch < 0 || cfg.StoreTimeout < 0 ||
		cfg.StartTimeout < 0:
		return errors.New("recording: limits must not be negative")
	}
	return nil
}

// failInterrupted marks recordings a previous process left active as
// incomplete, with an audit event each.
func (s *Service) failInterrupted(ctx context.Context) error {
	now := s.cfg.Now()
	var interrupted []string
	err := s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		interrupted, err = s.failInterruptedTx(ctx, tx, now)
		return err
	})
	if err != nil {
		return err
	}
	if len(interrupted) > 0 {
		s.cfg.Logger.Warn("recordings interrupted by restart marked incomplete", "count", len(interrupted))
	}
	return nil
}

func (s *Service) failInterruptedTx(ctx context.Context, tx *store.Tx, now time.Time) ([]string, error) {
	ids, err := tx.FailInterruptedRecordings(ctx, now, now.Add(s.cfg.Retention))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := s.audit(ctx, tx, "recording.incomplete", "", map[string]string{"recordingId": id, "code": CodeInterrupted}); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func setDefault[T int | int64 | time.Duration](v *T, fallback T) {
	if *v == 0 {
		*v = fallback
	}
}

// Begin starts recording a session that has not started its process yet.
// It waits at most StartTimeout for storage. A recording that cannot start
// is reported to owners as incomplete, and the session runs unrecorded.
func (s *Service) Begin(info session.Info) session.Recorder {
	if s.cfg.Disabled || s.isClosed() {
		return nil
	}
	id, err := newID()
	if err != nil {
		s.cfg.Logger.Error("recording id", "error", err)
		return nil
	}
	now := s.cfg.Now()
	meta := store.Recording{PublicID: id, TerminalID: info.PublicID, FormatVersion: FormatVersion, Codec: Codec,
		StartedAt: now, Rows: info.Rows, Cols: info.Cols}
	if err := s.createRecording(meta); err != nil {
		s.cfg.Logger.Error("start recording", "sessionId", info.PublicID, "error", err)
		return s.beginFailed(meta)
	}
	r := &recorder{s: s, id: id, terminalID: info.PublicID, start: now, wake: make(chan struct{}, 1)}
	return s.launch(r)
}

// createRecording stores meta as an active recording and audits it, within
// StartTimeout.
func (s *Service) createRecording(meta store.Recording) error {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.StartTimeout)
	defer cancel()
	return s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.CreateRecording(ctx, meta); err != nil {
			return err
		}
		return s.audit(ctx, tx, "recording.started", "", map[string]string{"recordingId": meta.PublicID, "terminalId": meta.TerminalID})
	})
}

// launch registers r and starts its writer, or finishes it as interrupted
// if the service closed meanwhile.
func (s *Service) launch(r *recorder) session.Recorder {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		r.finish(CodeInterrupted)
		return nil
	}
	s.active[r.terminalID] = r
	s.wg.Add(1)
	s.mu.Unlock()
	go r.run()
	return r
}

// beginFailed reports a recording that could not start: owners are warned
// at once and until the session ends, while its incomplete metadata is
// stored in the background if storage recovers in time.
func (s *Service) beginFailed(meta store.Recording) session.Recorder {
	st := Status{RecordingID: meta.PublicID, TerminalID: meta.TerminalID, State: store.RecordingIncomplete, Code: CodeStorage}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.reportLocked(st, true)
	s.wg.Add(1)
	s.mu.Unlock()
	go s.persistStartFailure(meta)
	return &failedRecorder{s: s, status: st}
}

// persistStartFailure stores meta as an incomplete recording with its
// audit. A start whose commit succeeded despite reporting an error already
// stored meta as active; that row is marked incomplete instead. Storage
// errors are logged, never shown to clients.
func (s *Service) persistStartFailure(meta store.Recording) {
	defer s.wg.Done()
	meta.FailureCode = CodeStorage
	meta.RetainUntil = meta.StartedAt.Add(s.cfg.Retention)
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.StoreTimeout)
	defer cancel()
	err := s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		if err := markOrCreateFailed(ctx, tx, meta); err != nil {
			return err
		}
		return s.audit(ctx, tx, "recording.incomplete", "", map[string]string{
			"recordingId": meta.PublicID, "terminalId": meta.TerminalID, "code": CodeStorage,
		})
	})
	if err != nil {
		s.cfg.Logger.Error("store failed recording", "recordingId", meta.PublicID, "sessionId", meta.TerminalID, "error", err)
	}
}

// markOrCreateFailed marks meta's row incomplete, creating it first if the
// failed start never stored it.
func markOrCreateFailed(ctx context.Context, tx *store.Tx, meta store.Recording) error {
	changed, err := tx.MarkRecordingIncomplete(ctx, meta.PublicID, CodeStorage, meta.StartedAt, meta.RetainUntil)
	if err != nil || changed {
		return err
	}
	if _, err := tx.Recording(ctx, meta.PublicID); !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return tx.CreateFailedRecording(ctx, meta)
}

// failedRecorder stands in for a recording that could not start. It drops
// events and withdraws the owners' warning when the session ends.
type failedRecorder struct {
	s      *Service
	status Status
}

func (f *failedRecorder) Output([]byte)               {}
func (f *failedRecorder) Resize(int, int)             {}
func (f *failedRecorder) Lifecycle(session.Lifecycle) {}
func (f *failedRecorder) End(session.Lifecycle) {
	f.s.forgetFailure(f.status.TerminalID, f.status.RecordingID)
}

// PresenceChanged records joins, leaves, and permission changes of a
// recorded terminal. It never blocks.
func (s *Service) PresenceChanged(terminalID string, e collab.Event) {
	event, ok := presenceEventName(e.Type)
	if !ok || e.Participant == nil {
		return
	}
	s.mu.Lock()
	r := s.active[terminalID]
	s.mu.Unlock()
	if r == nil {
		return
	}
	r.enqueue(Event{Kind: KindPresence, Presence: &Presence{Event: event, ParticipantID: e.Participant.ID,
		Role: string(e.Participant.Role), Reason: e.Reason}}, 0)
}

func presenceEventName(t string) (string, bool) {
	switch t {
	case collab.EventParticipantJoined:
		return "joined", true
	case collab.EventParticipantLeft:
		return "left", true
	case collab.EventPermissionChanged:
		return "permission_changed", true
	}
	return "", false
}

// Watch returns a channel that receives the terminal's recording failures,
// starting with any failure of its current session. Sends never block: a
// watcher that is not reading misses statuses. cancel stops the watch.
func (s *Service) Watch(terminalID string) (statuses <-chan Status, cancel func()) {
	ch := make(chan Status, 1)
	s.addWatcher(terminalID, ch)
	var once sync.Once
	return ch, func() { once.Do(func() { s.removeWatcher(terminalID, ch) }) }
}

func (s *Service) addWatcher(terminalID string, ch chan Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchers[terminalID] == nil {
		s.watchers[terminalID] = map[chan Status]struct{}{}
	}
	s.watchers[terminalID][ch] = struct{}{}
	if st, ok := s.failures[terminalID]; ok {
		ch <- st
	}
}

func (s *Service) removeWatcher(terminalID string, ch chan Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.watchers[terminalID], ch)
	if len(s.watchers[terminalID]) == 0 {
		delete(s.watchers, terminalID)
	}
}

// Close stops new recordings and waits for active ones to flush. Recordings
// still unfinished after ShutdownTimeout, or once ctx is done, are aborted
// and marked incomplete, and Close returns an error. It is idempotent.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	defer s.clearFailures()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	if s.waitForDeadline(ctx, done) {
		return nil
	}
	pending := s.abortActive()
	s.cancel()
	grace := time.NewTimer(abortGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
	}
	return fmt.Errorf("recording: %d recordings did not finish before the shutdown deadline", pending)
}

func (s *Service) clearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.failures)
}

// waitForDeadline reports whether done closed before ShutdownTimeout or ctx.
func (s *Service) waitForDeadline(ctx context.Context, done <-chan struct{}) bool {
	expired := make(chan struct{})
	var once sync.Once
	timer := s.cfg.AfterFunc(s.cfg.ShutdownTimeout, func() { once.Do(func() { close(expired) }) })
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-expired:
	case <-ctx.Done():
	}
	return false
}

// abortActive aborts every active recorder and returns how many there were.
func (s *Service) abortActive() int {
	s.mu.Lock()
	pending := make([]*recorder, 0, len(s.active))
	for _, r := range s.active {
		pending = append(pending, r)
	}
	s.mu.Unlock()
	s.cfg.Logger.Warn("recording shutdown deadline passed; aborting recordings", "recordings", len(pending))
	for _, r := range pending {
		r.abort()
	}
	return len(pending)
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// finished forgets r once its writer exits.
func (s *Service) finished(r *recorder) {
	s.mu.Lock()
	if s.active[r.terminalID] == r {
		delete(s.active, r.terminalID)
	}
	s.mu.Unlock()
	s.wg.Done()
}

// failed tells watchers that r stopped early, and remembers it for later
// watchers while the session is live.
func (s *Service) failed(r *recorder, code string) {
	st := Status{RecordingID: r.id, TerminalID: r.terminalID, State: store.RecordingIncomplete, Code: code}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.mu.Lock()
	ended := r.ended
	r.mu.Unlock()
	s.reportLocked(st, !ended)
}

// reportLocked sends st to the terminal's watchers without blocking and,
// if retain is set, keeps it for watchers that arrive later. Nothing is
// retained once the service is closing.
func (s *Service) reportLocked(st Status, retain bool) {
	if retain && !s.closed {
		s.failures[st.TerminalID] = st
	}
	for ch := range s.watchers[st.TerminalID] {
		select {
		case ch <- st:
		default:
		}
	}
}

// forgetFailure drops the retained failure of the terminal's recording
// recordingID once its session has ended.
func (s *Service) forgetFailure(terminalID, recordingID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures[terminalID].RecordingID == recordingID {
		delete(s.failures, terminalID)
	}
}

// audit records an event in tx. details carry only IDs, counts, and codes.
func (s *Service) audit(ctx context.Context, tx *store.Tx, eventType, remoteAddr string, details map[string]string) error {
	return tx.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: s.cfg.Now(), Type: eventType, RemoteAddr: remoteAddr, Details: details})
}

func (s *Service) auditNow(ctx context.Context, eventType, remoteAddr string, details map[string]string) error {
	return s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error { return s.audit(ctx, tx, eventType, remoteAddr, details) })
}

func countDetails(rec store.Recording) map[string]string {
	return map[string]string{
		"recordingId": rec.PublicID, "terminalId": rec.TerminalID,
		"events": strconv.FormatInt(rec.EventCount, 10), "chunks": strconv.FormatInt(rec.ChunkCount, 10),
		"bytes": strconv.FormatInt(rec.CompressedBytes, 10),
	}
}

// idBytes of randomness make an ID unguessable (144 bits).
const idBytes = 18

func newID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("recording id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
