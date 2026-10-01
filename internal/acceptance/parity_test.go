package acceptance_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/webassets"
)

func TestVersionReportsReleaseMetadata(t *testing.T) {
	r := webpty(t, env(dataDir(t)), "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "webpty "+acceptanceVersion+" (commit acceptance, built 2026-10-01T00:00:00Z,") {
		t.Fatalf("version = %d %q", r.code, r.stdout)
	}
}

// Like the legacy server, webpty with no flags listens on port 8000 and
// starts the user's shell; unlike it, only this machine can connect.
func TestDefaultBindIsLocalAndTheShellIsTheUsers(t *testing.T) {
	dir := dataDir(t)
	if r := webpty(t, env(dir), "doctor"); !strings.Contains(r.stdout, "network: 127.0.0.1:8000 ") {
		t.Errorf("default address is not 127.0.0.1:8000:\n%s", r.stdout)
	}

	s := start(t, env(dir, "SHELL=/bin/cat"))
	c := admin(t, s)
	if created := c.createTerminal(`{}`); created["command"] != "/bin/cat" {
		t.Errorf("default terminal = %v, want $SHELL", created["command"])
	}
	for _, ip := range externalIPv4(t) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprint(s.port)), time.Second)
		if err == nil {
			conn.Close()
			t.Errorf("%s:%d accepted a connection; the default bind must be loopback only", ip, s.port)
		}
	}
}

func externalIPv4(t *testing.T) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var ips []string
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
			ips = append(ips, ipNet.IP.String())
		}
	}
	return ips
}

func TestLegacyPortAndCommandFlags(t *testing.T) {
	for _, flags := range [][]string{{"-c", "/bin/echo"}, {"--cmd=/bin/echo"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			port := freePort(t)
			args := append([]string{"--port=" + fmt.Sprint(port)}, flags...)
			args = append(args, "--", "parity-$HOME", "`id`")
			s := startOn(t, port, env(dataDir(t)), args...)
			c := admin(t, s)
			created := c.createTerminal(`{}`)
			if created["command"] != "/bin/echo" {
				t.Fatalf("command = %v", created["command"])
			}
			term := c.attach(created["id"].(string))
			exit := term.until(func(msg map[string]any) bool { return msg["type"] == "exit" })
			if !strings.Contains(term.output.String(), "parity-$HOME `id`") {
				t.Errorf("output = %q, want the arguments passed literally, without a shell", term.output.String())
			}
			if exit["exitCode"] != float64(0) {
				t.Errorf("exit = %v", exit)
			}
		})
	}
	r := webpty(t, env(dataDir(t)), "--allowed-hosts", "example.com", "--password", "x")
	if r.code != 2 {
		t.Errorf("--password exit = %d, want a usage error", r.code)
	}
}

func TestHealthAndEmbeddedSPA(t *testing.T) {
	s := start(t, env(dataDir(t)))
	c := newClient(t, s.base)
	if code, _, body := c.raw("/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d %q", code, body)
	}
	code, header, body := c.raw("/admin")
	if _, err := fs.Stat(webassets.Dist(), "index.html"); err != nil {
		// A build without the frontend says how to fix itself.
		if code != http.StatusServiceUnavailable || !strings.Contains(body, "not built") {
			t.Fatalf("unbuilt UI = %d %q", code, body)
		}
		return
	}
	if code != http.StatusOK || !strings.Contains(body, `<div id="root">`) || !strings.Contains(header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("/admin = %d %q", code, body)
	}
	for _, ref := range strings.Split(body, `"/assets/`)[1:] {
		asset := "/assets/" + ref[:strings.IndexAny(ref, `"?`)]
		if code, _, _ := c.raw(asset); code != http.StatusOK {
			t.Errorf("%s = %d", asset, code)
		}
	}
}

func TestFirstRunForcesThePasswordChange(t *testing.T) {
	s := start(t, env(dataDir(t)))
	c := newClient(t, s.base)
	if code := c.login("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", code)
	}
	if code := c.login("CHANGEME"); code != http.StatusOK {
		t.Fatalf("CHANGEME = %d", code)
	}
	if _, session := c.do(http.MethodGet, "/api/v1/admin/session", ""); session["passwordChangeRequired"] != true {
		t.Fatalf("session = %v, want passwordChangeRequired", session)
	}
	if code, _ := c.do(http.MethodPost, "/api/v1/admin/terminals", `{}`); code != http.StatusForbidden {
		t.Fatalf("terminal before rotation = %d, want 403", code)
	}
	body, _ := json.Marshal(map[string]string{"currentPassword": "CHANGEME", "newPassword": rotatedPassword})
	if code, _ := c.do(http.MethodPost, "/api/v1/admin/password", string(body)); code != http.StatusNoContent {
		t.Fatalf("rotate = %d", code)
	}
	if code := newClient(t, s.base).login("CHANGEME"); code != http.StatusUnauthorized {
		t.Errorf("CHANGEME after rotation = %d, want 401", code)
	}
	if code := newClient(t, s.base).login(rotatedPassword); code != http.StatusOK {
		t.Errorf("new password = %d", code)
	}
}

func TestTerminalInputOutputResizeAndExit(t *testing.T) {
	s := start(t, env(dataDir(t)))
	c := admin(t, s)
	created := c.createTerminal(`{"command":"/bin/sh"}`)
	term := c.attach(created["id"].(string))
	term.send(`{"type":"resize","rows":33,"cols":101}`)
	term.input("stty size; echo marker-$((6*7)); exit 3\r")
	exit := term.until(func(msg map[string]any) bool { return msg["type"] == "exit" })
	if out := term.output.String(); !strings.Contains(out, "33 101") || !strings.Contains(out, "marker-42") {
		t.Errorf("output = %q, want the resized size and the shell's output", out)
	}
	if exit["exitCode"] != float64(3) {
		t.Errorf("exit = %v, want code 3", exit)
	}
}

func TestEditorAndViewerSharingAndRevocation(t *testing.T) {
	s := start(t, env(dataDir(t)))
	owner := admin(t, s)
	id := owner.createTerminal(`{"command":"/bin/cat"}`)["id"].(string)
	ownerTerm := owner.attach(id)

	guest := func(role string) (*client, string) {
		code, grant := owner.do(http.MethodPost, "/api/v1/admin/terminals/"+id+"/grants", fmt.Sprintf(`{"role":%q}`, role))
		if code != http.StatusCreated {
			t.Fatalf("grant %s = %d", role, code)
		}
		c := newClient(t, s.base)
		body, _ := json.Marshal(map[string]string{"token": grant["token"].(string)})
		if code, redeemed := c.do(http.MethodPost, "/api/v1/access/redeem", string(body)); code != http.StatusOK || redeemed["role"] != role {
			t.Fatalf("redeem %s = %d %v", role, code, redeemed)
		}
		return c, grant["grant"].(map[string]any)["id"].(string)
	}
	editor, _ := guest("editor")
	viewer, viewerGrant := guest("viewer")
	editorTerm := editor.attach(id)
	viewerTerm := viewer.attach(id)

	editorTerm.input("typed-by-editor\r")
	ownerTerm.untilOutput("typed-by-editor")
	viewerTerm.untilOutput("typed-by-editor")

	viewerTerm.input("typed-by-viewer\r")
	denied := viewerTerm.until(func(msg map[string]any) bool { return msg["type"] == "error" })
	if denied["code"] != "permission_denied" {
		t.Errorf("viewer input = %v, want permission_denied", denied)
	}

	if code, _ := owner.do(http.MethodDelete, "/api/v1/admin/terminals/"+id+"/grants/"+viewerGrant, ""); code != http.StatusOK {
		t.Fatalf("revoke = %d", code)
	}
	for {
		if _, err := viewerTerm.next(); err != nil {
			break
		}
	}
	if code, _ := viewer.do(http.MethodGet, "/api/v1/access/session", ""); code != http.StatusUnauthorized {
		t.Errorf("revoked viewer session = %d, want 401", code)
	}
	if strings.Contains(ownerTerm.output.String(), "typed-by-viewer") {
		t.Error("viewer input reached the terminal")
	}
}

func recordingFor(t *testing.T, c *client, terminalID string) map[string]any {
	t.Helper()
	var rec map[string]any
	eventually(t, "a complete recording", func() bool {
		_, body := c.do(http.MethodGet, "/api/v1/admin/recordings?terminalId="+terminalID, "")
		list, _ := body["recordings"].([]any)
		if len(list) != 1 {
			return false
		}
		rec = list[0].(map[string]any)
		return rec["status"] == "complete"
	})
	return rec
}

func TestRecordingPlaybackAndExport(t *testing.T) {
	s := start(t, env(dataDir(t)))
	c := admin(t, s)
	id := c.createTerminal(`{"command":"/bin/sh","args":["-c","echo recorded-parity; sleep 0.3"]}`)["id"].(string)
	rec := recordingFor(t, c, id)

	code, playback := c.do(http.MethodGet, "/api/v1/admin/recordings/"+rec["id"].(string)+"/events?limit=100", "")
	if code != http.StatusOK {
		t.Fatalf("events = %d", code)
	}
	encoded, _ := json.Marshal(playback["events"])
	if !strings.Contains(string(encoded), `"output"`) {
		t.Errorf("playback events = %s, want output events", encoded)
	}
	code, header, export := c.raw("/api/v1/admin/recordings/" + rec["id"].(string) + "/export")
	if code != http.StatusOK || !strings.HasPrefix(export, `{"version":2,`) || !strings.Contains(export, "recorded-parity") {
		t.Fatalf("export = %d %q", code, export)
	}
	if !strings.Contains(header.Get("Content-Disposition"), "attachment") {
		t.Errorf("export Content-Disposition = %q", header.Get("Content-Disposition"))
	}
}

func terminalState(t *testing.T, c *client, id string) string {
	t.Helper()
	code, info := c.do(http.MethodGet, "/api/v1/admin/terminals/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("get terminal = %d", code)
	}
	state, _ := info["state"].(string)
	return state
}

func TestGracefulShutdownAndRestartRecovery(t *testing.T) {
	dir := dataDir(t)
	s := start(t, env(dir))
	c := admin(t, s)
	graceful := c.createTerminal(`{"command":"/bin/sleep","args":["60"]}`)["id"].(string)
	if code := s.stop(); code != 0 {
		t.Fatalf("SIGTERM exit = %d:\n%s", code, s.stderr)
	}

	s = start(t, env(dir))
	c = newClient(t, s.base)
	if code := c.login("CHANGEME"); code != http.StatusUnauthorized {
		t.Errorf("CHANGEME after restart = %d; first-run setup must not repeat", code)
	}
	if code := c.login(rotatedPassword); code != http.StatusOK {
		t.Fatalf("login after restart = %d", code)
	}
	if state := terminalState(t, c, graceful); state != "terminated" {
		t.Errorf("terminal after graceful shutdown = %q, want terminated", state)
	}
	recordingFor(t, c, graceful)

	crashed := c.createTerminal(`{"command":"/bin/sleep","args":["60"]}`)["id"].(string)
	s.signal(syscall.SIGKILL)
	s = start(t, env(dir))
	c = newClient(t, s.base)
	if code := c.login(rotatedPassword); code != http.StatusOK {
		t.Fatalf("login after crash = %d", code)
	}
	if state := terminalState(t, c, crashed); state != "failed" {
		t.Errorf("terminal after crash = %q, want failed", state)
	}
	if code := s.stop(); code != 0 {
		t.Errorf("exit = %d", code)
	}
}

func TestBackupWhileServingAndOfflineRestore(t *testing.T) {
	dir := dataDir(t)
	s := start(t, env(dir))
	c := admin(t, s)
	before := c.createTerminal(`{"command":"/bin/sh","args":["-c","echo before-backup"]}`)["id"].(string)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if code, _ := c.do(http.MethodPost, "/api/v1/admin/terminals", `{"command":"/bin/sh","args":["-c",":"]}`); code != http.StatusCreated {
				t.Errorf("create terminal during backup = %d", code)
				return
			}
		}
	}()
	backups := filepath.Join(dataDir(t), "backups")
	if err := os.Mkdir(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		out := filepath.Join(backups, fmt.Sprintf("webpty-%d.db", i))
		if r := webpty(t, env(dir), "backup", "--output", out); r.code != 0 {
			close(stop)
			wg.Wait()
			t.Fatalf("backup during writes = %d %s", r.code, r.stderr)
		}
	}
	close(stop)
	wg.Wait()
	backup := filepath.Join(backups, "webpty-0.db")
	if r := webpty(t, env(dir), "backup", "--output", backup); r.code != 1 || !strings.Contains(r.stderr, "exists") {
		t.Errorf("overwrite = %d %q", r.code, r.stderr)
	}
	if r := webpty(t, env(dir), "restore", "--input", backup); r.code != 1 || !strings.Contains(r.stderr, "stop it first") {
		t.Errorf("restore while serving = %d %q", r.code, r.stderr)
	}
	after := c.createTerminal(`{"command":"/bin/sh","args":["-c",":"]}`)["id"].(string)
	if code := s.stop(); code != 0 {
		t.Fatalf("stop = %d", code)
	}

	r := webpty(t, env(dir), "restore", "--input", backup)
	if r.code != 0 || !strings.Contains(r.stdout, "pre-restore") {
		t.Fatalf("restore = %d %q %q", r.code, r.stdout, r.stderr)
	}
	s = start(t, env(dir))
	c = newClient(t, s.base)
	if code := c.login(rotatedPassword); code != http.StatusOK {
		t.Fatalf("login after restore = %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/api/v1/admin/terminals/"+before, ""); code != http.StatusOK {
		t.Errorf("terminal from before the backup = %d", code)
	}
	if code, _ := c.do(http.MethodGet, "/api/v1/admin/terminals/"+after, ""); code != http.StatusNotFound {
		t.Errorf("terminal created after the backup = %d, want 404", code)
	}
	if r := webpty(t, env(dir), "doctor"); !strings.Contains(r.stdout, "lock is held by a running webpty") {
		t.Errorf("doctor does not see the running server:\n%s", r.stdout)
	}
}
