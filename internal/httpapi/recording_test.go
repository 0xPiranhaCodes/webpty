package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/recording"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// failingStore fails recording appends while failing is set.
type failingStore struct {
	*store.Store
	failing atomic.Bool
}

func (f *failingStore) AppendRecordingChunk(ctx context.Context, id string, c store.RecordingChunk, now time.Time) error {
	if f.failing.Load() {
		return errors.New("disk I/O error at /var/lib/webpty/secret.db")
	}
	return f.Store.AppendRecordingChunk(ctx, id, c, now)
}

type recordingHarness struct {
	*collabHarness
	recordings *recording.Service
	faults     *failingStore
}

func newRecordingAPI(t *testing.T, configure func(*recording.Config)) *recordingHarness {
	t.Helper()
	starter := ptytest.NewStarter()
	h := &recordingHarness{collabHarness: &collabHarness{}}
	security := httpapi.SecuritySettings{}
	api := newAPIWith(t, security, func(service *auth.Service, s *store.Store) []httpapi.Option {
		h.faults = &failingStore{Store: s}
		cfg := recording.Config{
			Store: h.faults, ChunkEvents: 1, FlushInterval: 10 * time.Millisecond, Retention: time.Hour,
			Now: service.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if configure != nil {
			configure(&cfg)
		}
		svc, err := recording.New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		h.recordings = svc
		h.timers = &hubTimers{now: service.Now}
		h.hub = collab.NewHub(collab.Config{Now: service.Now, AfterFunc: h.timers.AfterFunc, Observer: svc})
		accessService, err := access.NewService(access.Config{Store: s, Listener: h.hub, Now: service.Now, SessionTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		h.access, h.store = accessService, s
		m, err := session.NewManager(context.Background(), session.Config{
			Store: s, Starter: starter, DefaultCommand: pty.Command{Path: "/bin/default"},
			KillGrace: 50 * time.Millisecond, Recorders: svc, OnEnd: h.hub.EndTerminal, Now: service.Now,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
			defer cancel()
			_ = m.Close(ctx)
			h.hub.Close()
			_ = svc.Close(ctx)
		})
		h.terminalHarness = &terminalHarness{manager: m, starter: starter}
		settings := httpapi.TerminalSettings{Access: accessService, Presence: h.hub, Recordings: svc}
		return []httpapi.Option{
			httpapi.WithTerminals(service, security, m, settings),
			httpapi.WithRecordings(service, security, svc),
		}
	})
	h.terminalHarness.apiHarness = api
	h.cookie = h.initialize()
	_, state := h.sessionState(h.cookie)
	h.csrf, _ = state["csrfToken"].(string)
	return h
}

func (h *recordingHarness) recordingOf(terminalID string) map[string]any {
	h.t.Helper()
	response := h.admin(http.MethodGet, "/api/v1/admin/recordings?terminalId="+terminalID, "")
	if response.Code != http.StatusOK {
		h.t.Fatalf("list = %d %s", response.Code, response.Body)
	}
	recs := decodeJSON(h.t, response)["recordings"].([]any)
	if len(recs) != 1 {
		h.t.Fatalf("terminal %s has %d recordings", terminalID, len(recs))
	}
	return recs[0].(map[string]any)
}

func (h *recordingHarness) waitStatus(terminalID, status string) map[string]any {
	h.t.Helper()
	var rec map[string]any
	eventually(h.t, "recording "+status, func() bool {
		response := h.admin(http.MethodGet, "/api/v1/admin/recordings?terminalId="+terminalID, "")
		recs, _ := decodeJSON(h.t, response)["recordings"].([]any)
		if len(recs) != 1 {
			return false
		}
		rec = recs[0].(map[string]any)
		return rec["status"] == status
	})
	return rec
}

// recordedTerminal runs a terminal that prints two lines and exits, and
// returns its completed recording.
func (h *recordingHarness) recordedTerminal() (terminalID string, rec map[string]any) {
	h.t.Helper()
	created, p := h.createTerminal(`{"rows":24,"cols":80}`)
	terminalID = created["id"].(string)
	server := h.server()
	owner := h.dial(server, terminalID, "")
	owner.readType("ready")
	owner.send(`{"type":"input","data":"typed-secret\r"}`)
	eventually(h.t, "input delivered", func() bool { return strings.Contains(p.Input(), "typed-secret") })
	p.Emit("hello\r\n")
	owner.readType("output")
	owner.send(`{"type":"resize","rows":30,"cols":100}`)
	eventually(h.t, "resize applied", func() bool { return len(p.Sizes()) > 1 })
	p.Emit("world\r\n")
	owner.readType("output")
	p.Exit(pty.ExitStatus{Code: 0})
	owner.readType("exit")
	return terminalID, h.waitStatus(terminalID, "complete")
}

func TestRecordingRoutesRequireAdminHostOriginAndCSRF(t *testing.T) {
	h := newRecordingAPI(t, nil)
	_, rec := h.recordedTerminal()
	id := rec["id"].(string)
	routes := []struct {
		method, path string
		mutating     bool
	}{
		{http.MethodGet, "/api/v1/admin/recordings", false},
		{http.MethodGet, "/api/v1/admin/recordings/" + id, false},
		{http.MethodGet, "/api/v1/admin/recordings/" + id + "/events", false},
		{http.MethodGet, "/api/v1/admin/recordings/" + id + "/export", false},
		{http.MethodDelete, "/api/v1/admin/recordings/" + id, true},
		{http.MethodPost, "/api/v1/admin/recordings/retention/run", true},
	}
	live, _ := h.createTerminal(`{}`)
	guest, _ := h.guest(live["id"].(string), "editor")
	for _, route := range routes {
		name := route.method + " " + route.path
		if r := h.do(request{method: route.method, path: route.path, origin: testOrigin}); r.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d, want 401", name, r.Code)
		}
		if r := h.do(request{method: route.method, path: route.path, origin: testOrigin, csrf: h.csrf,
			cookies: []*http.Cookie{guest}}); r.Code != http.StatusUnauthorized {
			t.Errorf("%s guest = %d, want 401", name, r.Code)
		}
		if r := h.do(request{method: route.method, path: route.path, origin: testOrigin, csrf: h.csrf,
			cookies: []*http.Cookie{h.cookie}, host: "evil.example"}); r.Code != http.StatusForbidden {
			t.Errorf("%s untrusted host = %d, want 403", name, r.Code)
		}
		if !route.mutating {
			continue
		}
		for what, r := range map[string]request{
			"no csrf":      {method: route.method, path: route.path, origin: testOrigin, cookies: []*http.Cookie{h.cookie}},
			"cross origin": {method: route.method, path: route.path, origin: "http://evil.example", csrf: h.csrf, cookies: []*http.Cookie{h.cookie}},
			"no origin":    {method: route.method, path: route.path, csrf: h.csrf, cookies: []*http.Cookie{h.cookie}},
		} {
			if response := h.do(r); response.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", name, what, response.Code)
			}
		}
	}
	if got := h.recordingOf(rec["terminalId"].(string))["status"]; got != "complete" {
		t.Fatalf("recording status after rejected requests = %v", got)
	}
}

func TestRecordingRoutesRejectBootstrapSessions(t *testing.T) {
	h := newRecordingAPI(t, nil)
	fresh := newAPIWith(t, httpapi.SecuritySettings{}, func(service *auth.Service, s *store.Store) []httpapi.Option {
		return []httpapi.Option{httpapi.WithRecordings(service, httpapi.SecuritySettings{}, h.recordings)}
	})
	bootstrap := fresh.bootstrapCookie()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/recordings"},
		{http.MethodPost, "/api/v1/admin/recordings/retention/run"},
	} {
		r := fresh.do(request{method: route.method, path: route.path, origin: testOrigin, cookies: []*http.Cookie{bootstrap}})
		if r.Code != http.StatusForbidden {
			t.Errorf("bootstrap %s %s = %d, want 403", route.method, route.path, r.Code)
		}
	}
}

func TestRecordingListDetailAndPagedEvents(t *testing.T) {
	h := newRecordingAPI(t, nil)
	terminalID, rec := h.recordedTerminal()
	id := rec["id"].(string)
	if rec["terminalId"] != terminalID || rec["rows"] != float64(24) || rec["cols"] != float64(80) ||
		rec["formatVersion"] != float64(1) || rec["endedAt"] == nil || rec["retainUntil"] == nil {
		t.Fatalf("list entry = %v", rec)
	}
	for _, field := range []string{"events", "chunks", "data"} {
		if _, ok := rec[field]; ok {
			t.Fatalf("list entry exposes %q: %v", field, rec)
		}
	}
	detail := h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id, "")
	if detail.Code != http.StatusOK || detail.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("detail = %d %v", detail.Code, detail.Header())
	}
	body := decodeJSON(t, detail)
	if body["id"] != id || body["status"] != "complete" || body["eventCount"] != rec["eventCount"] {
		t.Fatalf("detail = %v", body)
	}
	if strings.Contains(detail.Body.String(), "hello") {
		t.Fatalf("detail contains terminal output: %s", detail.Body)
	}

	var kinds []string
	var output strings.Builder
	path := "/api/v1/admin/recordings/" + id + "/events?limit=2"
	for pages := 0; path != ""; pages++ {
		if pages > 20 {
			t.Fatal("events never ended")
		}
		response := h.admin(http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("events = %d %s", response.Code, response.Body)
		}
		if strings.Contains(response.Body.String(), "typed-secret") {
			t.Fatalf("events expose input: %s", response.Body)
		}
		var page struct {
			Events []struct {
				Seq  int64           `json:"seq"`
				Kind string          `json:"kind"`
				Data json.RawMessage `json:"data"`
			} `json:"events"`
			Next *struct {
				AfterMS  int64 `json:"afterMs"`
				AfterSeq int64 `json:"afterSeq"`
			} `json:"next"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Events) > 2 {
			t.Fatalf("page has %d events, limit 2", len(page.Events))
		}
		for _, e := range page.Events {
			kinds = append(kinds, e.Kind)
			if e.Kind == "output" {
				var data struct{ Data []byte }
				if err := json.Unmarshal(e.Data, &data); err != nil {
					t.Fatal(err)
				}
				output.Write(data.Data)
			}
		}
		path = ""
		if page.Next != nil {
			path = "/api/v1/admin/recordings/" + id + "/events?limit=2&afterMs=" +
				itoa(page.Next.AfterMS) + "&afterSeq=" + itoa(page.Next.AfterSeq)
		}
	}
	if output.String() != "hello\r\nworld\r\n" {
		t.Fatalf("recorded output = %q", output.String())
	}
	if len(kinds) != int(rec["eventCount"].(float64)) || !contains(kinds, "resize") || !contains(kinds, "presence") ||
		kinds[len(kinds)-1] != "lifecycle" {
		t.Fatalf("event kinds = %v", kinds)
	}
	if h.auditCount("recording.playback") == 0 {
		t.Fatal("playback was not audited")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestRecordingEventsRejectInvalidQueries(t *testing.T) {
	h := newRecordingAPI(t, nil)
	_, rec := h.recordedTerminal()
	id := rec["id"].(string)
	for _, query := range []string{"afterMs=-1", "afterMs=soon", "limit=0x10", "limit=100000", "afterSeq=-2"} {
		assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/events?"+query, ""),
			http.StatusBadRequest, "invalid_argument")
	}
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings?limit=-1", ""), http.StatusBadRequest, "invalid_argument")
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/missing", ""), http.StatusNotFound, "not_found")
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/missing/events", ""), http.StatusNotFound, "not_found")
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/missing/export", ""), http.StatusNotFound, "not_found")
	assertErrorCode(t, h.admin(http.MethodDelete, "/api/v1/admin/recordings/missing", ""), http.StatusNotFound, "not_found")
}

func TestRecordingExportIsAsciicastAttachment(t *testing.T) {
	h := newRecordingAPI(t, nil)
	_, rec := h.recordedTerminal()
	id := rec["id"].(string)
	response := h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/export", "")
	if response.Code != http.StatusOK {
		t.Fatalf("export = %d %s", response.Code, response.Body)
	}
	if ct := response.Header().Get("Content-Type"); ct != "application/x-asciicast" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := response.Header().Get("Content-Disposition"); cd != `attachment; filename="webpty-`+id+`.cast"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("export headers = %v", response.Header())
	}
	body := response.Body.String()
	if strings.Contains(body, "typed-secret") || strings.Contains(body, "192.0.2.") || strings.Contains(body, h.cookie.Value) {
		t.Fatalf("export leaks input or secrets: %s", body)
	}
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Scan()
	var header map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil || header["version"] != float64(2) ||
		header["width"] != float64(80) || header["height"] != float64(24) {
		t.Fatalf("header = %s (%v)", scanner.Text(), err)
	}
	var events []string
	for scanner.Scan() {
		var e []any
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil || len(e) != 3 {
			t.Fatalf("event line %q: %v", scanner.Text(), err)
		}
		events = append(events, e[1].(string)+":"+e[2].(string))
	}
	want := []string{"o:hello\r\n", "r:100x30", "o:world\r\n"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %q, want %q", events, want)
	}
	if h.auditCount("recording.exported") != 1 {
		t.Fatal("export was not audited")
	}
}

func TestRecordingActiveIsNotExportableOrDeletable(t *testing.T) {
	h := newRecordingAPI(t, nil)
	created, _ := h.createTerminal(`{}`)
	rec := h.recordingOf(created["id"].(string))
	if rec["status"] != "recording" {
		t.Fatalf("recording = %v", rec)
	}
	id := rec["id"].(string)
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/export", ""), http.StatusConflict, "recording_active")
	assertErrorCode(t, h.admin(http.MethodDelete, "/api/v1/admin/recordings/"+id, ""), http.StatusConflict, "recording_active")
	if r := h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/events", ""); r.Code != http.StatusOK {
		t.Fatalf("events of an active recording = %d %s", r.Code, r.Body)
	}
}

func TestRecordingDeleteIsIdempotentAndLeavesTombstone(t *testing.T) {
	h := newRecordingAPI(t, nil)
	_, rec := h.recordedTerminal()
	id := rec["id"].(string)
	for i := 0; i < 2; i++ {
		response := h.admin(http.MethodDelete, "/api/v1/admin/recordings/"+id, "")
		if response.Code != http.StatusOK {
			t.Fatalf("delete #%d = %d %s", i+1, response.Code, response.Body)
		}
		if body := decodeJSON(t, response); body["status"] != "deleted" || body["deletedAt"] == nil || body["chunkCount"] != rec["chunkCount"] {
			t.Fatalf("delete #%d = %v", i+1, body)
		}
	}
	if detail := decodeJSON(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id, "")); detail["status"] != "deleted" {
		t.Fatalf("tombstone = %v", detail)
	}
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/events", ""), http.StatusGone, "recording_deleted")
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/export", ""), http.StatusGone, "recording_deleted")
	if n := h.auditCount("recording.deleted"); n != 1 {
		t.Fatalf("delete audits = %d, want 1", n)
	}
}

func TestRecordingRetentionRun(t *testing.T) {
	// Retention is shorter than the admin session so the clock can pass it.
	h := newRecordingAPI(t, func(c *recording.Config) { c.Retention = 20 * time.Minute })
	_, rec := h.recordedTerminal()
	active, _ := h.createTerminal(`{}`)
	run := func() float64 {
		t.Helper()
		response := h.admin(http.MethodPost, "/api/v1/admin/recordings/retention/run", "")
		if response.Code != http.StatusOK {
			t.Fatalf("retention = %d %s", response.Code, response.Body)
		}
		return decodeJSON(t, response)["deleted"].(float64)
	}
	if n := run(); n != 0 {
		t.Fatalf("retention before deadline deleted %v", n)
	}
	h.clock.Advance(30 * time.Minute)
	if n := run(); n != 1 {
		t.Fatalf("retention deleted %v, want 1", n)
	}
	if got := h.recordingOf(rec["terminalId"].(string))["status"]; got != "deleted" {
		t.Fatalf("expired recording status = %v", got)
	}
	if got := h.recordingOf(active["id"].(string))["status"]; got != "recording" {
		t.Fatalf("active recording status = %v", got)
	}
	if h.auditCount("recording.retention.deleted") != 1 {
		t.Fatal("retention delete was not audited")
	}
}

func TestCorruptRecordingIsRejectedAndMarkedIncomplete(t *testing.T) {
	h := newRecordingAPI(t, nil)
	terminalID, rec := h.recordedTerminal()
	id := rec["id"].(string)
	if _, err := h.store.DB().Exec(`DROP TRIGGER recording_chunks_are_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().Exec(`UPDATE recording_chunks SET checksum = zeroblob(32)
		WHERE chunk_index = 1 AND recording_id = (SELECT id FROM recordings WHERE public_id = ?)`, id); err != nil {
		t.Fatal(err)
	}
	response := h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/events", "")
	assertErrorCode(t, response, http.StatusUnprocessableEntity, "recording_corrupt")
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings/"+id+"/export", ""),
		http.StatusUnprocessableEntity, "recording_corrupt")
	if got := h.recordingOf(terminalID); got["status"] != "incomplete" || got["failureCode"] != "corrupt" {
		t.Fatalf("corrupt recording = %v", got)
	}
}

func TestRecordingFailureWarnsOwnerButNotGuests(t *testing.T) {
	h := newRecordingAPI(t, nil)
	h.faults.failing.Store(true)
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	rec := h.waitStatus(id, "incomplete")
	if rec["failureCode"] != "storage_error" {
		t.Fatalf("recording = %v", rec)
	}
	server := h.server()
	cookie, _ := h.guest(id, "editor")
	guest := h.dialAs(server, id, cookie)
	guest.readType("ready")

	owner := h.dial(server, id, "")
	owner.readType("ready")
	status := owner.skipTo("recording_status")
	if status["recordingId"] != rec["id"] || status["status"] != "incomplete" || status["code"] != "storage_error" {
		t.Fatalf("recording_status = %v", status)
	}
	if msg, _ := status["message"].(string); msg == "" || strings.Contains(msg, "secret.db") || strings.Contains(msg, "disk") {
		t.Fatalf("recording_status message = %q", msg)
	}

	// The live terminal keeps working after the recording failed.
	p.Emit("still live")
	if out := owner.readType("output"); out["data"] == "" {
		t.Fatalf("output = %v", out)
	}
	p.Exit(pty.ExitStatus{Code: 0})
	owner.readType("exit")
	for {
		msg := guest.read()
		if msg["type"] == "recording_status" {
			t.Fatalf("guest received %v", msg)
		}
		if msg["type"] == "exit" {
			break
		}
	}
}

func TestRecordingStartFailureIsReplayedToOwnersOnly(t *testing.T) {
	h := newRecordingAPI(t, nil)
	if _, err := h.store.DB().Exec(`CREATE TRIGGER fail_recording_create BEFORE INSERT ON recordings
		WHEN NEW.status = 'recording' BEGIN SELECT RAISE(ABORT, 'disk I/O error at /var/lib/webpty/secret.db'); END`); err != nil {
		t.Fatal(err)
	}
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	rec := h.waitStatus(id, "incomplete")
	if rec["failureCode"] != "storage_error" || rec["eventCount"] != float64(0) {
		t.Fatalf("recording = %v", rec)
	}
	server := h.server()
	cookie, _ := h.guest(id, "editor")
	guest := h.dialAs(server, id, cookie)
	guest.readType("ready")

	for i := 0; i < 2; i++ {
		owner := h.dial(server, id, "")
		owner.readType("ready")
		status := owner.skipTo("recording_status")
		if status["recordingId"] != rec["id"] || status["status"] != "incomplete" || status["code"] != "storage_error" {
			t.Fatalf("owner #%d recording_status = %v", i+1, status)
		}
		if msg, _ := status["message"].(string); strings.Contains(msg, "secret.db") || strings.Contains(msg, "disk") {
			t.Fatalf("recording_status message = %q", msg)
		}
		owner.conn.CloseNow()
	}

	p.Emit("still live")
	p.Exit(pty.ExitStatus{Code: 0})
	for {
		msg := guest.read()
		if msg["type"] == "recording_status" {
			t.Fatalf("guest received %v", msg)
		}
		if msg["type"] == "exit" {
			break
		}
	}
	if got := h.recordingOf(id); got["status"] != "incomplete" {
		t.Fatalf("recording after exit = %v", got)
	}
}

func TestRecordingFailureDuringSessionIsPushedToConnectedOwner(t *testing.T) {
	h := newRecordingAPI(t, nil)
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	owner := h.dial(server, id, "")
	owner.readType("ready")
	h.faults.failing.Store(true)
	p.Emit("x")
	status := owner.skipTo("recording_status")
	if status["status"] != "incomplete" || status["code"] != "storage_error" {
		t.Fatalf("recording_status = %v", status)
	}
	p.Emit("y")
	for {
		if out := owner.skipTo("output"); out["data"] == "eQ==" {
			break
		}
	}
}

// skipTo returns the next message of type want, discarding others.
func (c *wsClient) skipTo(want string) map[string]any {
	c.t.Helper()
	for {
		if msg := c.read(); msg["type"] == want {
			return msg
		}
	}
}

func TestCreateTerminalCanOptOutOfRecording(t *testing.T) {
	h := newRecordingAPI(t, nil)
	created, _ := h.createTerminal(`{"record":false}`)
	response := h.admin(http.MethodGet, "/api/v1/admin/recordings?terminalId="+created["id"].(string), "")
	if recs := decodeJSON(t, response)["recordings"].([]any); len(recs) != 0 {
		t.Fatalf("opted-out terminal has recordings %v", recs)
	}
	recorded, _ := h.createTerminal(`{"record":true}`)
	if rec := h.recordingOf(recorded["id"].(string)); rec["status"] != "recording" {
		t.Fatalf("recording = %v", rec)
	}
}
