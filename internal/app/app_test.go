package app_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0xPiranhaCodes/webpty/internal/app"
	"github.com/0xPiranhaCodes/webpty/internal/config"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	cfg.DatabasePath = filepath.Join(t.TempDir(), "webpty.db")
	return cfg
}

func TestNewServerAppliesTimeouts(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReadHeaderTimeout = time.Second
	cfg.ReadTimeout = 2 * time.Second
	cfg.WriteTimeout = 3 * time.Second
	cfg.IdleTimeout = 4 * time.Second

	server := app.NewServer(cfg, http.NotFoundHandler())

	if server.Addr != cfg.Address {
		t.Errorf("Addr = %q, want %q", server.Addr, cfg.Address)
	}
	if server.ReadHeaderTimeout != time.Second || server.ReadTimeout != 2*time.Second ||
		server.WriteTimeout != 3*time.Second || server.IdleTimeout != 4*time.Second {
		t.Errorf("timeouts = %v/%v/%v/%v", server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout, server.IdleTimeout)
	}
}

func TestServeWiresRoutesAndShutsDownGracefully(t *testing.T) {
	cfg := testConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, cfg, listener) }()

	health, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", health.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, base+"/api/v1/admin/login", strings.NewReader(`{"password":"CHANGEME"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", base)
	login, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Errorf("bootstrap login status = %d, want 200", login.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}

	if _, err := os.Stat(cfg.DatabasePath); err != nil {
		t.Errorf("database file not created: %v", err)
	}
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func (c *client) do(method, path, body string) *http.Response {
	c.t.Helper()
	request, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Origin", c.base)
	if c.csrf != "" {
		request.Header.Set("X-CSRF-Token", c.csrf)
	}
	response, err := c.http.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { response.Body.Close() })
	return response
}

func decode(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// serveAdmin starts Serve and returns an initialized admin client.
func serveAdmin(t *testing.T, cfg config.Config) (*client, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		done <- app.Serve(ctx, cfg, listener)
		close(exited)
	}()
	// The database lives in t.TempDir, which is removed after this cleanup:
	// Serve must have finished writing to it first.
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(time.Minute):
			t.Error("Serve did not return after cancel")
		}
	})

	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: "http://" + listener.Addr().String(), http: &http.Client{Jar: jar}}
	if response := c.do(http.MethodPost, "/api/v1/admin/login", `{"password":"CHANGEME"}`); response.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap login = %d", response.StatusCode)
	}
	if response := c.do(http.MethodPost, "/api/v1/admin/password", `{"currentPassword":"CHANGEME","newPassword":"a much better passphrase"}`); response.StatusCode != http.StatusNoContent {
		t.Fatalf("rotate = %d", response.StatusCode)
	}
	c.csrf, _ = decode(t, c.do(http.MethodGet, "/api/v1/admin/session", ""))["csrfToken"].(string)
	return c, cancel, done
}

func TestServeRunsTerminalOverWebSocketDespiteShortServerTimeouts(t *testing.T) {
	cfg := testConfig(t)
	cfg.Command = "/bin/cat"
	cfg.ReadTimeout = 2 * time.Second
	cfg.WriteTimeout = 2 * time.Second
	c, _, _ := serveAdmin(t, cfg)

	created := c.do(http.MethodPost, "/api/v1/admin/terminals", `{}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", created.StatusCode)
	}
	info := decode(t, created)
	if info["command"] != "/bin/cat" {
		t.Fatalf("created = %v, want the configured default command", info)
	}
	id := info["id"].(string)

	u, _ := url.Parse(c.base)
	header := http.Header{"Origin": {c.base}}
	for _, cookie := range c.http.Jar.Cookies(u) {
		header.Add("Cookie", cookie.Name+"="+cookie.Value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/v1/terminals/"+id+"/ws",
		&websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{"webpty.terminal.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	time.Sleep(2500 * time.Millisecond)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"after-timeout\r"}`)); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "after-timeout") {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, output.String())
		}
		var msg struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "output" {
			decoded, _ := base64.StdEncoding.DecodeString(msg.Data)
			output.Write(decoded)
		}
	}

	deleted := c.do(http.MethodDelete, "/api/v1/admin/terminals/"+id, "")
	if deleted.StatusCode != http.StatusOK || decode(t, deleted)["state"] != "terminated" {
		t.Fatalf("delete = %d", deleted.StatusCode)
	}
}

func TestServeShutdownTerminatesTerminalSessions(t *testing.T) {
	cfg := testConfig(t)
	cfg.TerminalKillGrace = 200 * time.Millisecond
	c, cancel, done := serveAdmin(t, cfg)
	created := c.do(http.MethodPost, "/api/v1/admin/terminals", `{"command":"/bin/sleep","args":["60"]}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", created.StatusCode)
	}
	id := decode(t, created)["id"].(string)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}

	s, err := store.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := s.TerminalSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != store.TerminalTerminated || info.EndedAt.IsZero() {
		t.Fatalf("after shutdown = %+v, want terminated", info)
	}
}

func TestServeFailsForUnopenableDatabase(t *testing.T) {
	cfg := testConfig(t)
	cfg.DatabasePath = filepath.Join(t.TempDir(), "missing", "dir", "webpty.db")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	if err := app.Serve(context.Background(), cfg, listener); err == nil {
		t.Fatal("Serve succeeded with an unopenable database")
	}
}

func TestServeShutdownKillsTermIgnoringSessionsAfterHTTPShutdownTimesOut(t *testing.T) {
	cfg := testConfig(t)
	cfg.ShutdownTimeout = 300 * time.Millisecond
	cfg.TerminalKillGrace = time.Second
	cfg.TerminalShutdownTimeout = 1500 * time.Millisecond
	c, cancel, done := serveAdmin(t, cfg)

	pidFile := filepath.Join(t.TempDir(), "pids")
	body, _ := json.Marshal(map[string]any{"command": "/bin/sh", "args": []string{"-c",
		`trap "" TERM HUP; sleep 60 & echo "$$ $!" > "$0.tmp" && mv "$0.tmp" "$0"; exec sleep 60`, pidFile}})
	created := c.do(http.MethodPost, "/api/v1/admin/terminals", string(body))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", created.StatusCode)
	}
	id := decode(t, created)["id"].(string)
	// A well-behaved session that needs a moment to exit on SIGTERM must
	// still get its graceful phase after HTTP shutdown timed out.
	readyFile := filepath.Join(t.TempDir(), "ready")
	body, _ = json.Marshal(map[string]any{"command": "/bin/sh", "args": []string{"-c",
		`trap "sleep 0.4; exit 7" TERM; : > "$0"; sleep 60 & wait`, readyFile}})
	created = c.do(http.MethodPost, "/api/v1/admin/terminals", string(body))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create graceful = %d", created.StatusCode)
	}
	graceful := decode(t, created)["id"].(string)
	var leader, background int
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, readyErr := os.Stat(readyFile)
		if data, err := os.ReadFile(pidFile); err == nil && readyErr == nil {
			if _, err := fmt.Sscanf(string(data), "%d %d", &leader, &background); err != nil {
				t.Fatalf("parse pids %q: %v", data, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never wrote its pids")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// An in-flight DELETE waits for the TERM-ignoring child, so graceful HTTP
	// shutdown spends its whole budget.
	deleteSent := make(chan struct{})
	go func() {
		request, _ := http.NewRequest(http.MethodDelete, c.base+"/api/v1/admin/terminals/"+id, nil)
		request.Header.Set("Origin", c.base)
		request.Header.Set("X-CSRF-Token", c.csrf)
		close(deleteSent)
		if response, err := c.http.Do(request); err == nil {
			response.Body.Close()
		}
	}()
	<-deleteSent
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return")
	}
	if syscall.Kill(leader, 0) == nil {
		t.Fatalf("session leader %d survived Serve", leader)
	}
	deadline = time.Now().Add(5 * time.Second)
	for syscall.Kill(background, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived Serve", background)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s, err := store.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := s.TerminalSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != store.TerminalTerminated || info.ExitSignal != "SIGKILL" {
		t.Fatalf("after shutdown = %+v, want terminated by SIGKILL", info)
	}
	info, err = s.TerminalSession(context.Background(), graceful)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != store.TerminalTerminated || info.ExitCode == nil || *info.ExitCode != 7 {
		t.Fatalf("graceful session = %+v, want it to exit 7 from its TERM handler", info)
	}
}

func TestServeSharesTerminalWithViewerGrant(t *testing.T) {
	cfg := testConfig(t)
	cfg.Command = "/bin/cat"
	c, _, _ := serveAdmin(t, cfg)
	created := c.do(http.MethodPost, "/api/v1/admin/terminals", `{}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", created.StatusCode)
	}
	id := decode(t, created)["id"].(string)

	granted := c.do(http.MethodPost, "/api/v1/admin/terminals/"+id+"/grants", `{"role":"viewer"}`)
	if granted.StatusCode != http.StatusCreated {
		t.Fatalf("grant = %d", granted.StatusCode)
	}
	token, _ := decode(t, granted)["token"].(string)

	jar, _ := cookiejar.New(nil)
	guest := &client{t: t, base: c.base, http: &http.Client{Jar: jar}}
	redeemed := guest.do(http.MethodPost, "/api/v1/access/redeem", fmt.Sprintf(`{"token":%q}`, token))
	if redeemed.StatusCode != http.StatusOK || decode(t, redeemed)["role"] != "viewer" {
		t.Fatalf("redeem = %d", redeemed.StatusCode)
	}
	if response := guest.do(http.MethodGet, "/api/v1/admin/terminals", ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("guest admin list = %d, want 401", response.StatusCode)
	}

	u, _ := url.Parse(c.base)
	header := http.Header{"Origin": {c.base}}
	for _, cookie := range jar.Cookies(u) {
		header.Add("Cookie", cookie.Name+"="+cookie.Value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/v1/terminals/"+id+"/ws",
		&websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{"webpty.terminal.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"x"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var msg struct{ Type, Code, Role string }
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "ready" && msg.Role != "viewer" {
			t.Fatalf("ready role = %q, want viewer", msg.Role)
		}
		if msg.Type == "error" {
			if msg.Code != "permission_denied" {
				t.Fatalf("error code = %q, want permission_denied", msg.Code)
			}
			return
		}
	}
}

func recordingsOf(t *testing.T, c *client, terminalID string) []any {
	t.Helper()
	response := c.do(http.MethodGet, "/api/v1/admin/recordings?terminalId="+terminalID, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list recordings = %d", response.StatusCode)
	}
	return decode(t, response)["recordings"].([]any)
}

func TestServeRecordsTerminalsAndFinishesRecordingsOnShutdown(t *testing.T) {
	cfg := testConfig(t)
	cfg.TerminalKillGrace = 200 * time.Millisecond
	c, cancel, done := serveAdmin(t, cfg)

	// The process outlives its output so the PTY is read before it closes.
	created := c.do(http.MethodPost, "/api/v1/admin/terminals",
		`{"command":"/bin/sh","args":["-c","echo recorded-hello; sleep 0.3"]}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", created.StatusCode)
	}
	echoID := decode(t, created)["id"].(string)
	var rec map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		if recs := recordingsOf(t, c, echoID); len(recs) == 1 {
			rec = recs[0].(map[string]any)
			if rec["status"] == "complete" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording never completed: %v", rec)
		}
		time.Sleep(20 * time.Millisecond)
	}
	export := c.do(http.MethodGet, "/api/v1/admin/recordings/"+rec["id"].(string)+"/export", "")
	var body strings.Builder
	if _, err := io.Copy(&body, export.Body); err != nil || export.StatusCode != http.StatusOK {
		t.Fatalf("export = %d %v", export.StatusCode, err)
	}
	if !strings.Contains(body.String(), "recorded-hello") || !strings.HasPrefix(body.String(), `{"version":2,`) {
		t.Fatalf("export = %q", body.String())
	}

	sleeping := c.do(http.MethodPost, "/api/v1/admin/terminals", `{"command":"/bin/sleep","args":["60"]}`)
	if sleeping.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", sleeping.StatusCode)
	}
	sleepID := decode(t, sleeping)["id"].(string)
	unrecorded := c.do(http.MethodPost, "/api/v1/admin/terminals", `{"command":"/bin/sleep","args":["60"],"record":false}`)
	if unrecorded.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", unrecorded.StatusCode)
	}
	unrecordedID := decode(t, unrecorded)["id"].(string)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return")
	}
	s, err := store.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recs, err := s.Recordings(context.Background(), store.RecordingFilter{TerminalID: sleepID, Limit: 10})
	if err != nil || len(recs) != 1 || recs[0].Status != store.RecordingComplete || recs[0].RetainUntil.IsZero() {
		t.Fatalf("recording after shutdown = %+v %v, want complete", recs, err)
	}
	if recs, err := s.Recordings(context.Background(), store.RecordingFilter{TerminalID: unrecordedID, Limit: 10}); err != nil || len(recs) != 0 {
		t.Fatalf("opted-out terminal recordings = %+v %v", recs, err)
	}
}

func TestServeWithRecordingDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.Command = "/bin/cat"
	cfg.RecordingEnabled = false
	c, _, _ := serveAdmin(t, cfg)
	created := c.do(http.MethodPost, "/api/v1/admin/terminals", `{}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", created.StatusCode)
	}
	if recs := recordingsOf(t, c, decode(t, created)["id"].(string)); len(recs) != 0 {
		t.Fatalf("recordings with recording disabled = %v", recs)
	}
}
