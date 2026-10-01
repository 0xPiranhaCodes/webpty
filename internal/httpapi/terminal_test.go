package httpapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const terminalWait = 5 * time.Second

type terminalHarness struct {
	*apiHarness
	manager *session.Manager
	starter *ptytest.Starter
	cookie  *http.Cookie
	csrf    string
}

func newTerminalAPI(t *testing.T, configure func(*session.Config), settings httpapi.TerminalSettings) *terminalHarness {
	t.Helper()
	starter := ptytest.NewStarter()
	var manager *session.Manager
	security := httpapi.SecuritySettings{}
	api := newAPIWith(t, security, func(service *auth.Service, s *store.Store) []httpapi.Option {
		cfg := session.Config{
			Store:          s,
			Starter:        starter,
			DefaultCommand: pty.Command{Path: "/bin/default"},
			KillGrace:      50 * time.Millisecond,
			Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if configure != nil {
			configure(&cfg)
		}
		m, err := session.NewManager(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		manager = m
		return []httpapi.Option{httpapi.WithTerminals(service, security, m, settings)}
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
		defer cancel()
		_ = manager.Close(ctx)
	})
	h := &terminalHarness{apiHarness: api, manager: manager, starter: starter}
	h.cookie = h.initialize()
	_, state := h.sessionState(h.cookie)
	h.csrf, _ = state["csrfToken"].(string)
	return h
}

func (h *terminalHarness) admin(method, path, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(request{method: method, path: path, body: body, origin: testOrigin, csrf: h.csrf, cookies: []*http.Cookie{h.cookie}})
}

func (h *terminalHarness) createTerminal(body string) (map[string]any, *ptytest.Process) {
	h.t.Helper()
	response := h.admin(http.MethodPost, "/api/v1/admin/terminals", body)
	if response.Code != http.StatusCreated {
		h.t.Fatalf("create status = %d body %s", response.Code, response.Body)
	}
	p := h.starter.Next(terminalWait)
	if p == nil {
		h.t.Fatal("no process started")
	}
	return decodeJSON(h.t, response), p
}

func assertErrorCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d body %s, want %d", response.Code, response.Body, status)
	}
	if body := decodeJSON(t, response); body["code"] != code {
		t.Fatalf("body = %v, want code %q", body, code)
	}
}

func TestCreateTerminalStartsExactCommand(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	body, p := h.createTerminal(`{"command":"/bin/echo","args":["a b","$(id)"],"rows":30,"cols":100}`)

	if p.Command.Path != "/bin/echo" || !reflect.DeepEqual(p.Command.Args, []string{"a b", "$(id)"}) {
		t.Fatalf("started %+v", p.Command)
	}
	if body["state"] != "running" || body["command"] != "/bin/echo" || body["rows"] != float64(30) || body["cols"] != float64(100) {
		t.Fatalf("body = %v", body)
	}
	if id, _ := body["id"].(string); id == "" {
		t.Fatalf("body has no id: %v", body)
	}
	if !reflect.DeepEqual(body["args"], []any{"a b", "$(id)"}) {
		t.Fatalf("args = %v", body["args"])
	}
}

func TestCreateTerminalDefaults(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	body, p := h.createTerminal(`{}`)
	if p.Command.Path != "/bin/default" || body["rows"] != float64(24) || body["cols"] != float64(80) {
		t.Fatalf("defaults: command %+v body %v", p.Command, body)
	}
}

func TestTerminalRoutesRequireAdminSession(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)

	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/admin/terminals", `{}`},
		{http.MethodGet, "/api/v1/admin/terminals", ""},
		{http.MethodGet, "/api/v1/admin/terminals/" + id, ""},
		{http.MethodDelete, "/api/v1/admin/terminals/" + id, ""},
		{http.MethodGet, "/api/v1/terminals/" + id + "/ws", ""},
	} {
		name := route.method + " " + route.path
		if response := h.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin}); response.Code != http.StatusUnauthorized {
			t.Errorf("%s without session = %d, want 401", name, response.Code)
		}
		if response := h.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin, host: "evil.example:8000", csrf: h.csrf, cookies: []*http.Cookie{h.cookie}}); response.Code != http.StatusForbidden {
			t.Errorf("%s with foreign host = %d, want 403", name, response.Code)
		}
	}
}

func TestTerminalRoutesRejectBootstrapSessions(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	// A fresh harness is still in bootstrap state.
	fresh := newAPIWith(t, httpapi.SecuritySettings{}, func(service *auth.Service, s *store.Store) []httpapi.Option {
		return []httpapi.Option{httpapi.WithTerminals(service, httpapi.SecuritySettings{}, h.manager, httpapi.TerminalSettings{})}
	})
	bootstrap := fresh.bootstrapCookie()
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/admin/terminals", `{}`},
		{http.MethodGet, "/api/v1/admin/terminals", ""},
		{http.MethodGet, "/api/v1/terminals/x/ws", ""},
	} {
		response := fresh.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin, cookies: []*http.Cookie{bootstrap}})
		if response.Code != http.StatusForbidden {
			t.Errorf("%s %s with bootstrap session = %d, want 403", route.method, route.path, response.Code)
		}
	}
}

func TestTerminalMutationsRequireCSRFAndOrigin(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)

	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/admin/terminals", `{}`},
		{http.MethodDelete, "/api/v1/admin/terminals/" + id, ""},
	} {
		cookies := []*http.Cookie{h.cookie}
		if response := h.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin, cookies: cookies}); response.Code != http.StatusForbidden {
			t.Errorf("%s %s without CSRF = %d, want 403", route.method, route.path, response.Code)
		}
		if response := h.do(request{method: route.method, path: route.path, body: route.body, origin: "http://evil.example", csrf: h.csrf, cookies: cookies}); response.Code != http.StatusForbidden {
			t.Errorf("%s %s cross-origin = %d, want 403", route.method, route.path, response.Code)
		}
		if response := h.do(request{method: route.method, path: route.path, body: route.body, csrf: h.csrf, cookies: cookies}); response.Code != http.StatusForbidden {
			t.Errorf("%s %s without origin = %d, want 403", route.method, route.path, response.Code)
		}
	}
	if p.Exited() {
		t.Fatal("rejected DELETE terminated the session")
	}
	if extra := h.starter.Next(10 * time.Millisecond); extra != nil {
		t.Fatal("rejected POST started a process")
	}
}

func TestCreateTerminalValidation(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	for name, body := range map[string]string{
		"unknown field":   `{"command":"/bin/sh","shell":true}`,
		"string command":  `{"command":["/bin/sh"]}`,
		"rows too large":  fmt.Sprintf(`{"rows":%d}`, session.MaxRows+1),
		"args no command": `{"args":["x"]}`,
		"trailing data":   `{} {}`,
	} {
		response := h.admin(http.MethodPost, "/api/v1/admin/terminals", body)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body %s, want 400", name, response.Code, response.Body)
		}
	}
	response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/terminals", body: `{}`, origin: testOrigin, csrf: h.csrf,
		cookies: []*http.Cookie{h.cookie}, headers: map[string]string{"Content-Type": "text/plain"}})
	if response.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain status = %d, want 415", response.Code)
	}
	assertErrorCode(t, h.admin(http.MethodPost, "/api/v1/admin/terminals", `{"cols":0,"rows":-4}`), http.StatusBadRequest, "invalid_argument")
}

func TestCreateTerminalMapsLimitAndStartFailure(t *testing.T) {
	h := newTerminalAPI(t, func(c *session.Config) { c.MaxSessions = 1 }, httpapi.TerminalSettings{})
	h.starter.FailNext(ptytest.ErrStart)
	response := h.admin(http.MethodPost, "/api/v1/admin/terminals", `{"command":"/missing"}`)
	assertErrorCode(t, response, http.StatusUnprocessableEntity, "process_failed")
	if strings.Contains(response.Body.String(), ptytest.ErrStart.Error()) {
		t.Errorf("start error details leaked: %s", response.Body)
	}

	h.createTerminal(`{}`)
	assertErrorCode(t, h.admin(http.MethodPost, "/api/v1/admin/terminals", `{}`), http.StatusTooManyRequests, "session_limit")
}

func TestListGetAndDeleteTerminals(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{"command":"/bin/cat"}`)
	id := created["id"].(string)

	list := h.admin(http.MethodGet, "/api/v1/admin/terminals", "")
	if list.Code != http.StatusOK || list.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list status = %d headers %v", list.Code, list.Header())
	}
	sessions, _ := decodeJSON(t, list)["sessions"].([]any)
	if len(sessions) != 1 || sessions[0].(map[string]any)["id"] != id {
		t.Fatalf("sessions = %v", sessions)
	}

	get := h.admin(http.MethodGet, "/api/v1/admin/terminals/"+id, "")
	if get.Code != http.StatusOK || decodeJSON(t, get)["state"] != "running" {
		t.Fatalf("get status = %d", get.Code)
	}
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/terminals/missing", ""), http.StatusNotFound, "not_found")

	deleted := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id, "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d body %s", deleted.Code, deleted.Body)
	}
	body := decodeJSON(t, deleted)
	if body["state"] != "terminated" || body["exitSignal"] != "SIGTERM" || body["endedAt"] == nil {
		t.Fatalf("delete body = %v", body)
	}
	if !p.Exited() || !p.Closed() {
		t.Fatal("process not reaped after DELETE")
	}
	if again := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id, ""); again.Code != http.StatusOK {
		t.Fatalf("second delete status = %d, want 200", again.Code)
	}
	assertErrorCode(t, h.admin(http.MethodDelete, "/api/v1/admin/terminals/missing", ""), http.StatusNotFound, "not_found")
}

// --- WebSocket ---

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func (h *terminalHarness) server() *httptest.Server {
	h.t.Helper()
	server := httptest.NewServer(h.handler)
	h.t.Cleanup(server.Close)
	return server
}

func dialOptions(server *httptest.Server, cookie *http.Cookie, origin string, protocols ...string) *websocket.DialOptions {
	header := http.Header{}
	if cookie != nil {
		header.Set("Cookie", cookie.Name+"="+cookie.Value)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	if protocols == nil {
		protocols = []string{httpapi.TerminalProtocol}
	}
	return &websocket.DialOptions{HTTPHeader: header, Subprotocols: protocols}
}

func wsURL(server *httptest.Server, id, query string) string {
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/terminals/" + id + "/ws"
	if query != "" {
		url += "?" + query
	}
	return url
}

func (h *terminalHarness) dial(server *httptest.Server, id, query string) *wsClient {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL(server, id, query), dialOptions(server, h.cookie, server.URL))
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		h.t.Fatalf("dial: %v (status %d)", err, status)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	return &wsClient{t: h.t, conn: conn}
}

func (c *wsClient) read() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	typ, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		c.t.Fatalf("message type = %v, want text", typ)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		c.t.Fatalf("decode %q: %v", data, err)
	}
	return msg
}

var presenceTypes = map[any]bool{
	"presence_snapshot": true, "participant_joined": true, "participant_left": true, "permission_changed": true,
}

// readType returns the next message, which must have type want. Presence
// and resize messages are skipped unless want is itself one of them.
func (c *wsClient) readType(want string) map[string]any {
	c.t.Helper()
	for {
		msg := c.read()
		if msg["type"] == want {
			return msg
		}
		if !presenceTypes[want] && presenceTypes[msg["type"]] {
			continue
		}
		if want != "resize" && msg["type"] == "resize" {
			continue
		}
		c.t.Fatalf("message = %v, want type %q", msg, want)
	}
}

func (c *wsClient) send(raw string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// closeStatus reads until the server closes the connection.
func (c *wsClient) closeStatus() (websocket.StatusCode, string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	for {
		_, _, err := c.conn.Read(ctx)
		if err == nil {
			continue
		}
		var closeErr websocket.CloseError
		if errors.As(err, &closeErr) {
			return closeErr.Code, closeErr.Reason
		}
		c.t.Fatalf("read ended without close frame: %v", err)
		return 0, ""
	}
}

func decodeOutput(t *testing.T, msg map[string]any) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(msg["data"].(string))
	if err != nil {
		t.Fatalf("output data %v is not base64: %v", msg["data"], err)
	}
	return string(data)
}

func TestWebSocketStreamsOutputAndForwardsInput(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()

	client := h.dial(server, id, "")
	if client.conn.Subprotocol() != httpapi.TerminalProtocol {
		t.Fatalf("subprotocol = %q", client.conn.Subprotocol())
	}
	ready := client.readType("ready")
	if ready["version"] != float64(1) || ready["seq"] != float64(0) {
		t.Fatalf("ready = %v", ready)
	}
	if info, _ := ready["session"].(map[string]any); info["id"] != id || info["state"] != "running" {
		t.Fatalf("ready session = %v", ready["session"])
	}

	p.Emit("hello \x1b[31mworld\xff")
	output := client.readType("output")
	if output["seq"] != float64(1) || decodeOutput(t, output) != "hello \x1b[31mworld\xff" {
		t.Fatalf("output = %v", output)
	}

	client.send(`{"type":"input","data":"ls -la\r"}`)
	client.send(`{"type":"resize","rows":40,"cols":120}`)
	client.send(`{"type":"ping"}`)
	client.readType("pong")
	eventually(t, "input written to the PTY", func() bool { return p.Input() == "ls -la\r" })
	if sizes := p.Sizes(); sizes[len(sizes)-1] != (pty.Size{Rows: 40, Cols: 120}) {
		t.Fatalf("sizes = %v", sizes)
	}

	client.send(`{"type":"resize","rows":0,"cols":120}`)
	if msg := client.readType("error"); msg["code"] != "invalid_resize" {
		t.Fatalf("error = %v", msg)
	}
	client.send(`{"type":"ping"}`)
	client.readType("pong")

	p.Exit(pty.ExitStatus{Code: 7})
	exit := client.readType("exit")
	if exit["state"] != "exited" || exit["exitCode"] != float64(7) {
		t.Fatalf("exit = %v", exit)
	}
	if code, _ := client.closeStatus(); code != websocket.StatusNormalClosure {
		t.Fatalf("close code = %v, want normal", code)
	}
}

func TestWebSocketReconnectReplaysAfterSeq(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()

	first := h.dial(server, id, "")
	first.readType("ready")
	for _, chunk := range []string{"a", "b", "c"} {
		p.Emit(chunk)
		first.readType("output")
	}
	first.conn.Close(websocket.StatusNormalClosure, "")

	if info, err := h.manager.Get(context.Background(), id); err != nil || info.State != store.TerminalRunning {
		t.Fatalf("session after disconnect = %+v, %v", info, err)
	}

	second := h.dial(server, id, "afterSeq=1")
	if ready := second.readType("ready"); ready["seq"] != float64(3) {
		t.Fatalf("ready = %v", ready)
	}
	for _, want := range []struct {
		seq  float64
		data string
	}{{2, "b"}, {3, "c"}} {
		msg := second.readType("output")
		if msg["seq"] != want.seq || decodeOutput(t, msg) != want.data {
			t.Fatalf("replayed %v, want seq %v %q", msg, want.seq, want.data)
		}
	}
}

func TestWebSocketReplayGapIsTypedError(t *testing.T) {
	h := newTerminalAPI(t, func(c *session.Config) {
		c.ReplayBytes = session.EventOverhead + 1
	}, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	first := h.dial(server, id, "")
	first.readType("ready")
	for _, chunk := range []string{"a", "b"} {
		p.Emit(chunk)
		first.readType("output")
	}

	gap := h.dial(server, id, "afterSeq=0")
	msg := gap.readType("error")
	if msg["code"] != "replay_gap" || msg["firstSeq"] != float64(2) || msg["lastSeq"] != float64(2) {
		t.Fatalf("error = %v", msg)
	}
	if code, _ := gap.closeStatus(); code != httpapi.CloseReplayGap {
		t.Fatalf("close code = %v, want %v", code, httpapi.CloseReplayGap)
	}
}

func TestWebSocketUpgradeValidation(t *testing.T) {
	h := newTerminalAPI(t, func(c *session.Config) { c.MaxViewers = 1 }, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()

	dialStatus := func(url string, opts *websocket.DialOptions) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
		defer cancel()
		conn, response, err := websocket.Dial(ctx, url, opts)
		if err == nil {
			conn.CloseNow()
			return http.StatusSwitchingProtocols
		}
		if response == nil {
			t.Fatalf("dial %s: %v", url, err)
		}
		return response.StatusCode
	}

	for name, tc := range map[string]struct {
		url    string
		opts   *websocket.DialOptions
		status int
	}{
		"no cookie":          {wsURL(server, id, ""), dialOptions(server, nil, server.URL), http.StatusUnauthorized},
		"cross origin":       {wsURL(server, id, ""), dialOptions(server, h.cookie, "http://evil.example"), http.StatusForbidden},
		"missing origin":     {wsURL(server, id, ""), dialOptions(server, h.cookie, ""), http.StatusForbidden},
		"no subprotocol":     {wsURL(server, id, ""), dialOptions(server, h.cookie, server.URL, "other"), http.StatusBadRequest},
		"bad afterSeq":       {wsURL(server, id, "afterSeq=-1"), dialOptions(server, h.cookie, server.URL), http.StatusBadRequest},
		"unknown session":    {wsURL(server, "missing", ""), dialOptions(server, h.cookie, server.URL), http.StatusNotFound},
		"subprotocol casing": {wsURL(server, id, ""), dialOptions(server, h.cookie, server.URL, strings.ToUpper(httpapi.TerminalProtocol)), http.StatusBadRequest},
	} {
		if got := dialStatus(tc.url, tc.opts); got != tc.status {
			t.Errorf("%s: status = %d, want %d", name, got, tc.status)
		}
	}

	viewer := h.dial(server, id, "")
	viewer.readType("ready")
	if got := dialStatus(wsURL(server, id, ""), dialOptions(server, h.cookie, server.URL)); got != http.StatusTooManyRequests {
		t.Errorf("second viewer status = %d, want 429", got)
	}

	p.Exit(pty.ExitStatus{Code: 0})
	viewer.readType("exit")
	deadline := time.Now().Add(terminalWait)
	for {
		got := dialStatus(wsURL(server, id, ""), dialOptions(server, h.cookie, server.URL))
		if got == http.StatusConflict {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ended session status = %d, want 409", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWebSocketRejectsMalformedMessages(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{MaxMessageBytes: 1024})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()

	for name, tc := range map[string]struct {
		send      func(*wsClient)
		code      websocket.StatusCode
		wantError bool
	}{
		"invalid json":    {func(c *wsClient) { c.send(`{"type":`) }, websocket.StatusPolicyViolation, true},
		"unknown type":    {func(c *wsClient) { c.send(`{"type":"exec"}`) }, websocket.StatusPolicyViolation, true},
		"unknown field":   {func(c *wsClient) { c.send(`{"type":"ping","extra":1}`) }, websocket.StatusPolicyViolation, true},
		"missing data":    {func(c *wsClient) { c.send(`{"type":"input"}`) }, websocket.StatusPolicyViolation, true},
		"resize no cols":  {func(c *wsClient) { c.send(`{"type":"resize","rows":10}`) }, websocket.StatusPolicyViolation, true},
		"trailing values": {func(c *wsClient) { c.send(`{"type":"ping"}{"type":"ping"}`) }, websocket.StatusPolicyViolation, true},
		"duplicate type":  {func(c *wsClient) { c.send(`{"type":"ping","type":"input","data":"x"}`) }, websocket.StatusPolicyViolation, true},
		"duplicate data":  {func(c *wsClient) { c.send(`{"type":"input","data":"a","data":"b"}`) }, websocket.StatusPolicyViolation, true},
		"case-folded key": {func(c *wsClient) { c.send(`{"type":"input","DATA":"x"}`) }, websocket.StatusPolicyViolation, true},
		"null data on ping": {func(c *wsClient) { c.send(`{"type":"ping","data":null}`) },
			websocket.StatusPolicyViolation, true},
		"null rows on ping": {func(c *wsClient) { c.send(`{"type":"ping","rows":null}`) },
			websocket.StatusPolicyViolation, true},
		"size on input": {func(c *wsClient) { c.send(`{"type":"input","data":"x","cols":null}`) },
			websocket.StatusPolicyViolation, true},
		"null type":       {func(c *wsClient) { c.send(`{"type":null}`) }, websocket.StatusPolicyViolation, true},
		"null message":    {func(c *wsClient) { c.send(`null`) }, websocket.StatusPolicyViolation, true},
		"array message":   {func(c *wsClient) { c.send(`[{"type":"ping"}]`) }, websocket.StatusPolicyViolation, true},
		"fractional rows": {func(c *wsClient) { c.send(`{"type":"resize","rows":10.5,"cols":10}`) }, websocket.StatusPolicyViolation, true},
		"string rows":     {func(c *wsClient) { c.send(`{"type":"resize","rows":"10","cols":10}`) }, websocket.StatusPolicyViolation, true},
		"invalid utf8": {func(c *wsClient) {
			ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
			defer cancel()
			_ = c.conn.Write(ctx, websocket.MessageText, []byte("{\"type\":\"input\",\"data\":\"\xff\"}"))
		}, websocket.StatusInvalidFramePayloadData, false},
		"binary": {func(c *wsClient) {
			ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
			defer cancel()
			_ = c.conn.Write(ctx, websocket.MessageBinary, []byte(`{"type":"ping"}`))
		}, websocket.StatusUnsupportedData, false},
		"too large": {func(c *wsClient) {
			ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
			defer cancel()
			_ = c.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"`+strings.Repeat("x", 2048)+`"}`))
		}, websocket.StatusMessageTooBig, false},
	} {
		t.Run(name, func(t *testing.T) {
			client := h.dial(server, id, "")
			client.t = t
			client.readType("ready")
			tc.send(client)
			if tc.wantError {
				if msg := client.readType("error"); msg["code"] != "invalid_message" {
					t.Fatalf("error = %v", msg)
				}
			}
			if code, _ := client.closeStatus(); code != tc.code {
				t.Fatalf("close code = %v, want %v", code, tc.code)
			}
		})
	}
	if p.Input() != "" {
		t.Fatalf("malformed messages reached the PTY: %q", p.Input())
	}
	if p.Exited() {
		t.Fatal("protocol violations terminated the session")
	}
}

func TestWebSocketClosesWhenAdminSessionEnds(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{PingInterval: 20 * time.Millisecond})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	client := h.dial(server, id, "")
	client.readType("ready")

	logout := h.admin(http.MethodPost, "/api/v1/admin/logout", "")
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d", logout.Code)
	}
	if code, _ := client.closeStatus(); code != httpapi.CloseUnauthorized {
		t.Fatalf("close code = %v, want %v", code, httpapi.CloseUnauthorized)
	}
}

func TestWebSocketSurvivesShortServerTimeouts(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)

	server := httptest.NewUnstartedServer(h.handler)
	server.Config.ReadTimeout = 150 * time.Millisecond
	server.Config.WriteTimeout = 150 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	client := h.dial(server, id, "")
	client.readType("ready")
	time.Sleep(400 * time.Millisecond)
	p.Emit("late output")
	if got := decodeOutput(t, client.readType("output")); got != "late output" {
		t.Fatalf("output = %q", got)
	}
	client.send(`{"type":"ping"}`)
	client.readType("pong")
}

func TestWebSocketCloseReasons(t *testing.T) {
	for err, want := range map[error]websocket.StatusCode{
		session.ErrSlowConsumer: httpapi.CloseSlowConsumer,
		io.EOF:                  websocket.StatusNormalClosure,
	} {
		code, reason := httpapi.CloseStatusFor(err)
		if code != want || reason == "" {
			t.Errorf("CloseStatusFor(%v) = %v %q, want %v with a reason", err, code, reason, want)
		}
	}
}
