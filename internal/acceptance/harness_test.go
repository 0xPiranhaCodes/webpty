package acceptance_test

import (
	"bytes"
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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	acceptanceVersion = "0.0.0-acceptance"
	rotatedPassword   = "a much better passphrase"
)

// binary is the webpty executable built from this tree with release-style
// linker flags; every test drives it as a separate process.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "webpty-acceptance-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "webpty")
	pkg := "github.com/0xPiranhaCodes/webpty/internal/buildinfo"
	build := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X "+pkg+".version="+acceptanceVersion+" -X "+pkg+".commit=acceptance -X "+pkg+".date=2026-10-01T00:00:00Z",
		"-o", binary, "./cmd/webpty")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build webpty: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is a minimal process environment: nothing WEBPTY_* leaks in from the
// developer's shell.
func env(dir string, extra ...string) []string {
	return append([]string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + dir,
		"SHELL=/bin/sh",
		"WEBPTY_DATABASE_PATH=" + filepath.Join(dir, "webpty.db"),
		"WEBPTY_TERMINAL_KILL_GRACE=300ms",
		"WEBPTY_RECORDING_FLUSH_INTERVAL=100ms",
	}, extra...)
}

func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type result struct {
	code           int
	stdout, stderr string
}

// webpty runs a short-lived command to completion.
func webpty(t *testing.T, environment []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = environment
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{code, stdout.String(), stderr.String()}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type server struct {
	t      *testing.T
	cmd    *exec.Cmd
	base   string
	port   int
	stderr *syncBuffer
	exited chan struct{}
	err    error
}

// start runs webpty serve on a free loopback port and waits for /healthz.
func start(t *testing.T, environment []string, args ...string) *server {
	t.Helper()
	port := freePort(t)
	return startOn(t, port, environment, append([]string{"-p", fmt.Sprint(port)}, args...)...)
}

func startOn(t *testing.T, port int, environment []string, args ...string) *server {
	t.Helper()
	s := &server{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), port: port, stderr: &syncBuffer{}, exited: make(chan struct{})}
	s.cmd = exec.Command(binary, args...)
	s.cmd.Env = environment
	s.cmd.Stdout, s.cmd.Stderr = s.stderr, s.stderr
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		s.err = s.cmd.Wait()
		close(s.exited)
	}()
	t.Cleanup(func() { s.signal(syscall.SIGKILL) })
	deadline := time.Now().Add(15 * time.Second)
	for {
		if response, err := http.Get(s.base + "/healthz"); err == nil {
			response.Body.Close()
			return s
		}
		select {
		case <-s.exited:
			t.Fatalf("webpty exited before becoming healthy: %v\n%s", s.err, s.stderr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("webpty never became healthy:\n%s", s.stderr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (s *server) signal(sig syscall.Signal) {
	select {
	case <-s.exited:
	default:
		_ = s.cmd.Process.Signal(sig)
		<-s.exited
	}
}

// stop sends SIGTERM and returns the exit code.
func (s *server) stop() int {
	s.t.Helper()
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(30 * time.Second):
		s.t.Fatalf("webpty ignored SIGTERM:\n%s", s.stderr)
	}
	return s.cmd.ProcessState.ExitCode()
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (c *client) do(method, path, body string) (int, map[string]any) {
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
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded
}

func (c *client) raw(path string) (int, http.Header, string) {
	c.t.Helper()
	response, err := c.http.Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, response.Header, string(body)
}

func (c *client) refreshCSRF() {
	c.t.Helper()
	_, session := c.do(http.MethodGet, "/api/v1/admin/session", "")
	c.csrf, _ = session["csrfToken"].(string)
}

// login signs in with password and returns the status code.
func (c *client) login(password string) int {
	c.t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	code, _ := c.do(http.MethodPost, "/api/v1/admin/login", string(body))
	if code == http.StatusOK {
		c.refreshCSRF()
	}
	return code
}

// admin completes first-run setup on a fresh server.
func admin(t *testing.T, s *server) *client {
	t.Helper()
	c := newClient(t, s.base)
	if code := c.login("CHANGEME"); code != http.StatusOK {
		t.Fatalf("bootstrap login = %d", code)
	}
	body, _ := json.Marshal(map[string]string{"currentPassword": "CHANGEME", "newPassword": rotatedPassword})
	if code, _ := c.do(http.MethodPost, "/api/v1/admin/password", string(body)); code != http.StatusNoContent {
		t.Fatalf("rotate = %d", code)
	}
	c.refreshCSRF()
	return c
}

func (c *client) createTerminal(body string) map[string]any {
	c.t.Helper()
	code, created := c.do(http.MethodPost, "/api/v1/admin/terminals", body)
	if code != http.StatusCreated {
		c.t.Fatalf("create terminal %s = %d %v", body, code, created)
	}
	return created
}

type terminal struct {
	t      *testing.T
	conn   *websocket.Conn
	ctx    context.Context
	output strings.Builder
}

func (c *client) attach(id string) *terminal {
	c.t.Helper()
	u, _ := url.Parse(c.base)
	header := http.Header{"Origin": {c.base}}
	for _, cookie := range c.http.Jar.Cookies(u) {
		header.Add("Cookie", cookie.Name+"="+cookie.Value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	c.t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/v1/terminals/"+id+"/ws",
		&websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{"webpty.terminal.v1"}})
	if err != nil {
		c.t.Fatalf("attach %s: %v", id, err)
	}
	c.t.Cleanup(func() { conn.CloseNow() })
	term := &terminal{t: c.t, conn: conn, ctx: ctx}
	term.until(func(msg map[string]any) bool { return msg["type"] == "ready" })
	return term
}

func (term *terminal) send(message string) {
	term.t.Helper()
	if err := term.conn.Write(term.ctx, websocket.MessageText, []byte(message)); err != nil {
		term.t.Fatalf("send %s: %v", message, err)
	}
}

func (term *terminal) input(text string) {
	term.t.Helper()
	data, _ := json.Marshal(map[string]string{"type": "input", "data": text})
	term.send(string(data))
}

// next reads one message, appending terminal output to term.output.
func (term *terminal) next() (map[string]any, error) {
	_, data, err := term.conn.Read(term.ctx)
	if err != nil {
		return nil, err
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		term.t.Fatal(err)
	}
	if msg["type"] == "output" {
		decoded, _ := base64.StdEncoding.DecodeString(msg["data"].(string))
		term.output.Write(decoded)
	}
	return msg, nil
}

func (term *terminal) until(done func(map[string]any) bool) map[string]any {
	term.t.Helper()
	for {
		msg, err := term.next()
		if err != nil {
			term.t.Fatalf("read: %v (output %q)", err, term.output.String())
		}
		if done(msg) {
			return msg
		}
	}
}

func (term *terminal) untilOutput(fragment string) {
	term.t.Helper()
	term.until(func(map[string]any) bool { return strings.Contains(term.output.String(), fragment) })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
