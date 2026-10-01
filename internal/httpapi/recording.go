package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

type recordingAPI struct {
	recordings *recording.Service
	logger     *slog.Logger
}

// WithRecordings mounts the admin recording API: metadata, validated
// playback, asciicast export, deletion, and retention.
func WithRecordings(service *auth.Service, security SecuritySettings, recordings *recording.Service) Option {
	a := &adminAPI{service: service, security: security}
	api := &recordingAPI{recordings: recordings, logger: slog.Default()}
	read, mutate := a.adminRoutes()
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/v1/admin/recordings", read(api.list))
		mux.Handle("GET /api/v1/admin/recordings/{id}", read(api.get))
		mux.Handle("GET /api/v1/admin/recordings/{id}/events", read(api.events))
		mux.Handle("GET /api/v1/admin/recordings/{id}/export", read(api.export))
		mux.Handle("DELETE /api/v1/admin/recordings/{id}", mutate(api.delete))
		mux.Handle("POST /api/v1/admin/recordings/retention/run", mutate(api.retention))
	}
}

// recordingJSON is recording metadata. It never includes events.
type recordingJSON struct {
	ID                string     `json:"id"`
	TerminalID        string     `json:"terminalId"`
	Status            string     `json:"status"`
	FormatVersion     int        `json:"formatVersion"`
	StartedAt         time.Time  `json:"startedAt"`
	EndedAt           *time.Time `json:"endedAt"`
	DurationMS        int64      `json:"durationMs"`
	Rows              int        `json:"rows"`
	Cols              int        `json:"cols"`
	EventCount        int64      `json:"eventCount"`
	ChunkCount        int64      `json:"chunkCount"`
	CompressedBytes   int64      `json:"compressedBytes"`
	UncompressedBytes int64      `json:"uncompressedBytes"`
	FailureCode       string     `json:"failureCode,omitempty"`
	RetainUntil       *time.Time `json:"retainUntil"`
	DeletedAt         *time.Time `json:"deletedAt"`
}

func toRecordingJSON(rec store.Recording) recordingJSON {
	optional := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		utc := t.UTC()
		return &utc
	}
	return recordingJSON{
		ID: rec.PublicID, TerminalID: rec.TerminalID, Status: string(rec.Status), FormatVersion: rec.FormatVersion,
		StartedAt: rec.StartedAt.UTC(), EndedAt: optional(rec.EndedAt), DurationMS: rec.DurationMS,
		Rows: rec.Rows, Cols: rec.Cols, EventCount: rec.EventCount, ChunkCount: rec.ChunkCount,
		CompressedBytes: rec.CompressedBytes, UncompressedBytes: rec.UncompressedBytes, FailureCode: rec.FailureCode,
		RetainUntil: optional(rec.RetainUntil), DeletedAt: optional(rec.DeletedAt),
	}
}

func (api *recordingAPI) list(w http.ResponseWriter, r *http.Request) {
	limit, ok := queryInt(w, r, "limit")
	if !ok {
		return
	}
	query := r.URL.Query()
	list, err := api.recordings.Recordings(r.Context(), recording.ListQuery{
		TerminalID: query.Get("terminalId"), Limit: int(limit), Before: query.Get("before"),
	})
	if err != nil {
		api.writeError(w, "list recordings", err)
		return
	}
	body := struct {
		Recordings []recordingJSON `json:"recordings"`
		NextCursor *string         `json:"nextCursor"`
	}{Recordings: make([]recordingJSON, 0, len(list.Recordings))}
	if list.NextCursor != "" {
		body.NextCursor = &list.NextCursor
	}
	for _, rec := range list.Recordings {
		body.Recordings = append(body.Recordings, toRecordingJSON(rec))
	}
	writeJSON(w, http.StatusOK, body)
}

func (api *recordingAPI) get(w http.ResponseWriter, r *http.Request) {
	rec, err := api.recordings.Recording(r.Context(), r.PathValue("id"))
	if err != nil {
		api.writeError(w, "get recording", err)
		return
	}
	writeJSON(w, http.StatusOK, toRecordingJSON(rec))
}

type cursorJSON struct {
	AfterMS  int64 `json:"afterMs"`
	AfterSeq int64 `json:"afterSeq"`
}

func (api *recordingAPI) events(w http.ResponseWriter, r *http.Request) {
	var q recording.Query
	var limit int64
	for _, param := range []struct {
		name   string
		target *int64
	}{{"afterMs", &q.AfterMS}, {"afterSeq", &q.AfterSeq}, {"limit", &limit}} {
		value, ok := queryInt(w, r, param.name)
		if !ok {
			return
		}
		*param.target = value
	}
	q.Limit = int(limit)
	page, err := api.recordings.Events(r.Context(), r.PathValue("id"), q, remoteIP(r))
	if err != nil {
		api.writeError(w, "recording events", err)
		return
	}
	body := struct {
		Recording recordingJSON     `json:"recording"`
		Events    []recording.Event `json:"events"`
		Next      *cursorJSON       `json:"next"`
	}{Recording: toRecordingJSON(page.Recording), Events: page.Events}
	if body.Events == nil {
		body.Events = []recording.Event{}
	}
	if page.Next != nil {
		body.Next = &cursorJSON{AfterMS: page.Next.AfterMS, AfterSeq: page.Next.AfterSeq}
	}
	writeJSON(w, http.StatusOK, body)
}

// export streams an asciicast v2 file. The recording is fully validated
// before the response starts; a failure while streaming aborts the
// connection so a truncated file is never mistaken for a complete one.
func (api *recordingAPI) export(w http.ResponseWriter, r *http.Request) {
	x, err := api.recordings.PrepareExport(r.Context(), r.PathValue("id"), remoteIP(r))
	if err != nil {
		api.writeError(w, "export recording", err)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/x-asciicast")
	header.Set("Content-Disposition", `attachment; filename="webpty-`+x.Recording().PublicID+`.cast"`)
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if err := x.WriteTo(r.Context(), &rollingDeadlineWriter{w: w, rc: http.NewResponseController(w)}); err != nil {
		if r.Context().Err() == nil {
			api.logger.Error("recording export interrupted", "recordingId", x.Recording().PublicID, "error", err)
		}
		panic(http.ErrAbortHandler)
	}
}

// exportWriteTimeout bounds each write of an export. The deadline moves
// forward with every write, so only a stalled client, not a long recording,
// ends the response.
const exportWriteTimeout = 30 * time.Second

type rollingDeadlineWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (d *rollingDeadlineWriter) Write(p []byte) (int, error) {
	if err := d.rc.SetWriteDeadline(time.Now().Add(exportWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	return d.w.Write(p)
}

func (api *recordingAPI) delete(w http.ResponseWriter, r *http.Request) {
	rec, err := api.recordings.Delete(r.Context(), r.PathValue("id"), remoteIP(r))
	if err != nil {
		api.writeError(w, "delete recording", err)
		return
	}
	writeJSON(w, http.StatusOK, toRecordingJSON(rec))
}

func (api *recordingAPI) retention(w http.ResponseWriter, r *http.Request) {
	deleted, err := api.recordings.RunRetention(r.Context())
	if err != nil {
		api.writeError(w, "run recording retention", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Deleted int `json:"deleted"`
	}{deleted})
}

// queryInt parses an optional non-negative decimal query parameter,
// answering an invalid value with invalid_argument.
func queryInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		writeCodedError(w, http.StatusBadRequest, "invalid_argument", name+" must be a non-negative integer")
		return 0, false
	}
	return value, true
}

// writeError maps recording errors to stable status codes and error codes.
// Messages are fixed strings so storage details never leak.
func (api *recordingAPI) writeError(w http.ResponseWriter, operation string, err error) {
	status, code, message := http.StatusInternalServerError, "internal", "internal error"
	switch {
	case errors.Is(err, recording.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "recording not found"
	case errors.Is(err, recording.ErrDeleted):
		status, code, message = http.StatusGone, "recording_deleted", "recording was deleted"
	case errors.Is(err, recording.ErrActive):
		status, code, message = http.StatusConflict, "recording_active", "recording is still in progress"
	case errors.Is(err, recording.ErrCorrupt):
		status, code, message = http.StatusUnprocessableEntity, "recording_corrupt", "recording failed validation and was marked incomplete"
	case errors.Is(err, recording.ErrInvalidArgument):
		status, code, message = http.StatusBadRequest, "invalid_argument", "invalid recording request"
	default:
		api.logger.Error("recording api", "operation", operation, "error", err)
	}
	writeCodedError(w, status, code, message)
}

func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{message, code})
}
