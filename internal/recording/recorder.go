package recording

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// eventOverhead is the fixed per-event cost charged against the queue.
const eventOverhead = 64

// codeStopped marks a recording ended by someone else, such as playback
// finding it corrupt.
const codeStopped = "stopped"

// recorder queues one session's events for its writer goroutine. Enqueueing
// takes only the recorder's own lock and never waits for storage.
type recorder struct {
	s          *Service
	id         string
	terminalID string
	start      time.Time
	wake       chan struct{}

	mu         sync.Mutex
	queue      []Event
	queueBytes int
	seq        int64
	lastMS     int64
	ended      bool
	failure    string
	aborted    bool
	flushDue   bool
	timer      Timer
	timerGen   uint64
}

var _ session.Recorder = (*recorder)(nil)

func (r *recorder) Output(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(data) > 0 {
		n := min(len(data), MaxOutputEventBytes)
		r.enqueueLocked(Event{Kind: KindOutput, Output: data[:n]}, n)
		data = data[n:]
	}
}

func (r *recorder) Resize(rows, cols int) {
	r.enqueue(Event{Kind: KindResize, Rows: rows, Cols: cols}, 0)
}

func (r *recorder) Lifecycle(l session.Lifecycle) {
	r.enqueue(Event{Kind: KindLifecycle, Lifecycle: toLifecycle(l)}, 0)
}

func (r *recorder) End(l session.Lifecycle) {
	r.mu.Lock()
	r.enqueueLocked(Event{Kind: KindLifecycle, Lifecycle: toLifecycle(l)}, 0)
	r.ended = true
	r.signal()
	r.mu.Unlock()
	r.s.forgetFailure(r.terminalID, r.id)
}

func toLifecycle(l session.Lifecycle) *Lifecycle {
	return &Lifecycle{State: string(l.State), ExitCode: l.ExitCode, Signal: l.Signal, Reason: l.Reason}
}

func (r *recorder) enqueue(e Event, size int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueueLocked(e, size)
}

// enqueueLocked stamps e with its sequence and offset and queues it. Once
// the queue would overflow, the recording stops accepting events.
func (r *recorder) enqueueLocked(e Event, size int) {
	if r.ended || r.failure != "" {
		return
	}
	cost := size + eventOverhead
	if r.queueBytes+cost > r.s.cfg.QueueBytes {
		r.failure = CodeQueueOverflow
		r.signal()
		return
	}
	ms := r.s.cfg.Now().Sub(r.start).Milliseconds()
	if ms < r.lastMS {
		ms = r.lastMS
	}
	r.lastMS = ms
	r.seq++
	e.Seq, e.OffsetMS = r.seq, ms
	r.queue = append(r.queue, e)
	r.queueBytes += cost
	r.signal()
}

func (r *recorder) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *recorder) abort() {
	r.mu.Lock()
	r.aborted = true
	r.mu.Unlock()
	r.signal()
}

func (r *recorder) isAborted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aborted
}

type batch struct {
	events   []Event
	ended    bool
	failure  string
	flushDue bool
	aborted  bool
}

func (r *recorder) take() batch {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.queue) == 0 && !r.ended && r.failure == "" && !r.flushDue && !r.aborted {
		r.mu.Unlock()
		<-r.wake
		r.mu.Lock()
	}
	b := batch{events: r.queue, ended: r.ended, failure: r.failure, flushDue: r.flushDue, aborted: r.aborted}
	r.queue, r.queueBytes, r.flushDue = nil, 0, false
	return b
}

func (r *recorder) armTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		return
	}
	r.timerGen++
	gen := r.timerGen
	r.timer = r.s.cfg.AfterFunc(r.s.cfg.FlushInterval, func() {
		r.mu.Lock()
		if r.timerGen == gen {
			r.timer = nil
			r.flushDue = true
		}
		r.mu.Unlock()
		r.signal()
	})
}

func (r *recorder) stopTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timerGen++
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

// run encodes queued events into chunks and stores them until the session
// ends or the recording fails.
func (r *recorder) run() {
	defer r.s.finished(r)
	defer r.stopAccepting()
	w := chunkWriter{r: r}
	for {
		b := r.take()
		if b.aborted {
			r.finish(CodeShutdownTimeout)
			return
		}
		code := w.addAll(b.events)
		if code == CodeEncoding || (code == "" && b.failure != "") {
			// Events accepted before the failure are intact; keep them.
			if flushed := w.flush(); flushed != "" && code == "" {
				code = flushed
			}
		} else if code == "" && (b.flushDue || b.ended) {
			code = w.flush()
		}
		if code == "" {
			code = b.failure
		}
		switch {
		case code == codeStopped:
			r.stopped()
			return
		case code != "":
			r.finish(code)
			return
		case b.ended:
			r.finish("")
			return
		case w.b.count > 0:
			r.armTimer()
		}
	}
}

// stopAccepting drops anything queued after the writer has exited.
func (r *recorder) stopAccepting() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure == "" {
		r.failure = codeStopped
	}
	r.queue, r.queueBytes = nil, 0
}

type chunkWriter struct {
	r      *recorder
	b      chunkBuilder
	index  int64
	stored int64
}

// addAll encodes events, storing each chunk once it is full. It returns a
// failure code, or "".
func (w *chunkWriter) addAll(events []Event) string {
	cfg := w.r.s.cfg
	for _, e := range events {
		line, err := w.r.s.encode(e)
		if err != nil {
			cfg.Logger.Error("encode recording event", "recordingId", w.r.id, "kind", e.Kind, "error", err)
			return CodeEncoding
		}
		if w.b.count > 0 && w.b.len()+len(line) > cfg.ChunkBytes {
			if code := w.flush(); code != "" {
				return code
			}
		}
		w.b.add(line, e)
		if w.b.count >= int64(cfg.ChunkEvents) || w.b.len() >= cfg.ChunkBytes {
			if code := w.flush(); code != "" {
				return code
			}
		}
	}
	return ""
}

func (w *chunkWriter) flush() string {
	if w.b.count == 0 {
		return ""
	}
	w.r.stopTimer()
	cfg := w.r.s.cfg
	c, err := w.b.seal(w.index)
	if err != nil {
		cfg.Logger.Error("seal recording chunk", "recordingId", w.r.id, "error", err)
		return CodeEncoding
	}
	if w.stored+c.UncompressedBytes > cfg.MaxRecordingBytes {
		return CodeSizeLimit
	}
	ctx, cancel := context.WithTimeout(w.r.s.ctx, cfg.StoreTimeout)
	err = cfg.Store.AppendRecordingChunk(ctx, w.r.id, c, cfg.Now())
	cancel()
	switch {
	case err == nil:
		w.index++
		w.stored += c.UncompressedBytes
		return ""
	case w.r.isAborted():
		return CodeShutdownTimeout
	case errors.Is(err, store.ErrRecordingNotActive):
		return codeStopped
	default:
		cfg.Logger.Error("store recording chunk", "recordingId", w.r.id, "error", err)
		return CodeStorage
	}
}

// finish ends the recording as complete, or as incomplete with code.
func (r *recorder) finish(code string) {
	r.stopTimer()
	s := r.s
	now := s.cfg.Now()
	end := store.RecordingEnd{Status: store.RecordingComplete, EndedAt: now, RetainUntil: now.Add(s.cfg.Retention)}
	eventType := "recording.completed"
	if code != "" {
		end.Status, end.FailureCode, eventType = store.RecordingIncomplete, code, "recording.incomplete"
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.StoreTimeout)
	defer cancel()
	err := s.cfg.Store.WithTx(ctx, func(tx *store.Tx) error {
		rec, err := tx.FinishRecording(ctx, r.id, end)
		if err != nil {
			return err
		}
		details := countDetails(rec)
		if code != "" {
			details["code"] = code
		}
		return s.audit(ctx, tx, eventType, "", details)
	})
	switch {
	case errors.Is(err, store.ErrRecordingNotActive):
		r.stopped()
		return
	case err != nil:
		s.cfg.Logger.Error("finish recording", "recordingId", r.id, "error", err)
	}
	if code != "" {
		s.cfg.Logger.Warn("recording incomplete", "recordingId", r.id, "sessionId", r.terminalID, "code", code)
		s.failed(r, code)
	}
}

// stopped reports a recording that was ended elsewhere.
func (r *recorder) stopped() {
	ctx, cancel := context.WithTimeout(context.Background(), r.s.cfg.StoreTimeout)
	defer cancel()
	rec, err := r.s.cfg.Store.Recording(ctx, r.id)
	if err == nil && rec.Status == store.RecordingIncomplete {
		r.s.failed(r, rec.FailureCode)
	}
}
