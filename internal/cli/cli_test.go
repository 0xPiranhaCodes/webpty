package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/cli"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

var healthyAssets = fstest.MapFS{
	"index.html":    {Data: []byte(`<script type="module" src="/assets/app.js"></script>`)},
	"assets/app.js": {Data: []byte("1")},
}

// syncBuffer is a bytes.Buffer safe for a serve goroutine and the test.
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

type result struct {
	code           int
	stdout, stderr string
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func environment(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func run(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr syncBuffer
	code := cli.CLI{Lookup: environment(env), Stdout: &stdout, Stderr: &stderr, Assets: healthyAssets}.Run(context.Background(), args)
	return result{code, stdout.String(), stderr.String()}
}

func dbEnv(path string) map[string]string {
	return map[string]string{"WEBPTY_DATABASE_PATH": path, "SHELL": "/bin/sh"}
}

func TestHelpListsCommandsAndServeFlags(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		r := run(t, nil, args...)
		if r.code != 0 {
			t.Fatalf("%v exit = %d, stderr %q", args, r.code, r.stderr)
		}
		for _, want := range []string{"serve", "version", "doctor", "backup", "restore", "-port", "-cmd", "-public-origin"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%v help lacks %q:\n%s", args, want, r.stdout)
			}
		}
	}
	if r := run(t, nil, "backup", "--help"); r.code != 0 || !strings.Contains(r.stdout+r.stderr, "-output") {
		t.Errorf("backup --help = %d %q %q", r.code, r.stdout, r.stderr)
	}
}

func TestVersionReportsDevelopmentMetadata(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		r := run(t, nil, args...)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "webpty dev (commit ") {
			t.Errorf("%v = %d %q", args, r.code, r.stdout)
		}
	}
	r := run(t, nil, "version", "--json")
	var info map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil || info["version"] != "dev" || info["commit"] == "" || info["platform"] == "" {
		t.Errorf("version --json = %q (%v)", r.stdout, err)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	r := run(t, nil, "frobnicate")
	if r.code != 2 || !strings.Contains(r.stderr, "unknown command") {
		t.Errorf("= %d %q", r.code, r.stderr)
	}
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

// withPort adds -p after an optional "serve" and before any flags, so it
// never lands among the command's arguments after "--".
func withPort(args []string, port int) []string {
	out := []string{}
	if len(args) > 0 && args[0] == "serve" {
		out, args = append(out, "serve"), args[1:]
	}
	return append(append(out, "-p", fmt.Sprint(port)), args...)
}

func serve(t *testing.T, env map[string]string, args ...string) (stop func() result) {
	t.Helper()
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- cli.CLI{Lookup: environment(env), Stdout: &stdout, Stderr: &stderr, Assets: healthyAssets}.
			Run(ctx, withPort(args, port))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			response.Body.Close()
			break
		}
		select {
		case code := <-done:
			cancel()
			t.Fatalf("serve exited %d: %s", code, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server never became healthy: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := false
	stop = func() result {
		if !stopped {
			stopped = true
			cancel()
		}
		select {
		case code := <-done:
			return result{code, stdout.String(), stderr.String()}
		case <-time.After(30 * time.Second):
			t.Fatal("serve did not stop")
			return result{}
		}
	}
	t.Cleanup(func() {
		if !stopped {
			stop()
		}
	})
	return stop
}

func TestServeIsTheDefaultCommandAndStopsCleanly(t *testing.T) {
	db := filepath.Join(privateDir(t), "webpty.db")
	for _, args := range [][]string{nil, {"serve"}} {
		stop := serve(t, dbEnv(db), args...)
		if r := stop(); r.code != 0 {
			t.Fatalf("%v exit = %d: %s", args, r.code, r.stderr)
		}
	}
}

func TestServeWarnsAboutDeprecatedAllowedHosts(t *testing.T) {
	db := filepath.Join(privateDir(t), "webpty.db")
	stop := serve(t, dbEnv(db), "--allowed-hosts", "example.com")
	r := stop()
	if r.code != 0 || !strings.Contains(r.stderr, "--allowed-hosts is deprecated") {
		t.Fatalf("= %d %q", r.code, r.stderr)
	}
}

func TestServeConfigurationErrorsAreUsageErrors(t *testing.T) {
	db := filepath.Join(privateDir(t), "webpty.db")
	r := run(t, dbEnv(db), "--password", "hunter2-secret")
	if r.code != 2 || !strings.Contains(r.stderr, "CHANGEME") || strings.Contains(r.stderr, "hunter2-secret") {
		t.Errorf("--password = %d %q", r.code, r.stderr)
	}
	r = run(t, dbEnv(db), "--address", "0.0.0.0:0")
	if r.code != 2 || !strings.Contains(r.stderr, "WEBPTY_PUBLIC_ORIGIN") {
		t.Errorf("exposed address = %d %q", r.code, r.stderr)
	}
}

func TestServeRefusesADatabaseInUse(t *testing.T) {
	db := filepath.Join(privateDir(t), "webpty.db")
	release, err := store.LockDatabase(db)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	r := run(t, dbEnv(db), "-p", fmt.Sprint(freePort(t)))
	if r.code != 1 || !strings.Contains(r.stderr, "in use") {
		t.Errorf("= %d %q", r.code, r.stderr)
	}
}

func TestDoctorPassesAHealthySetup(t *testing.T) {
	env := dbEnv(filepath.Join(privateDir(t), "webpty.db"))
	env["WEBPTY_ADDRESS"] = "127.0.0.1:0"
	r := run(t, env, "doctor")
	if r.code != 0 || !strings.Contains(r.stdout, "0 failed") {
		t.Fatalf("doctor = %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestDoctorFailsWithNonZeroExit(t *testing.T) {
	env := dbEnv(filepath.Join(t.TempDir(), "missing", "webpty.db"))
	env["WEBPTY_ADDRESS"] = "127.0.0.1:0"
	r := run(t, env, "doctor")
	if r.code != 1 || !strings.Contains(r.stdout, "[FAIL] database directory") {
		t.Fatalf("doctor = %d\n%s", r.code, r.stdout)
	}
}

func TestDoctorReportsAnInvalidConfiguration(t *testing.T) {
	env := dbEnv(filepath.Join(privateDir(t), "webpty.db"))
	env["WEBPTY_ADDRESS"] = "0.0.0.0:8000"
	r := run(t, env, "doctor")
	if r.code != 1 || !strings.Contains(r.stdout, "[FAIL] configuration") {
		t.Fatalf("doctor = %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestOutputNeverContainsSecrets(t *testing.T) {
	dir := privateDir(t)
	env := dbEnv(filepath.Join(dir, "webpty.db"))
	env["WEBPTY_ADDRESS"] = "127.0.0.1:0"
	env["AWS_SECRET_ACCESS_KEY"] = "env-secret-value-123"
	env["WEBPTY_CHILD_ENV_PASSTHROUGH"] = "AWS_SECRET_ACCESS_KEY"
	stop := serve(t, env, "--cmd", "/bin/echo", "--", "argument-secret-456")
	served := stop()
	outputs := []result{
		served,
		run(t, env, "doctor", "--cmd", "/bin/echo", "--", "argument-secret-456"),
		run(t, env, "backup", "--output", filepath.Join(dir, "b.db")),
		run(t, env, "restore", "--input", filepath.Join(dir, "b.db")),
		run(t, env, "version"),
	}
	for i, r := range outputs {
		for _, secret := range []string{"env-secret-value-123", "argument-secret-456"} {
			if strings.Contains(r.stdout+r.stderr, secret) {
				t.Errorf("output %d contains %q:\n%s%s", i, secret, r.stdout, r.stderr)
			}
		}
	}
}

func seeded(t *testing.T, events ...string) string {
	t.Helper()
	path := filepath.Join(privateDir(t), "webpty.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, e := range events {
		if err := s.AppendAuditEvent(context.Background(), store.AuditEvent{OccurredAt: time.Now(), Type: e}); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func events(t *testing.T, path string) string {
	t.Helper()
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Type)
	}
	return strings.Join(names, ",")
}

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	db := seeded(t, "before")
	backup := filepath.Join(privateDir(t), "webpty-backup.db")
	r := run(t, dbEnv(db), "backup", "--output", backup)
	if r.code != 0 || !strings.Contains(r.stdout, backup) || !strings.Contains(r.stdout, "schema version") {
		t.Fatalf("backup = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := run(t, dbEnv(db), "backup", "--output", backup); r.code != 1 || !strings.Contains(r.stderr, "exists") {
		t.Errorf("second backup = %d %q, want a refusal to overwrite", r.code, r.stderr)
	}

	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AppendAuditEvent(context.Background(), store.AuditEvent{OccurredAt: time.Now(), Type: "after"})
	s.Close()

	r = run(t, nil, "restore", "--input", backup, "--database", db)
	if r.code != 0 || !strings.Contains(r.stdout, ".pre-restore-") {
		t.Fatalf("restore = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got := events(t, db); got != "before" {
		t.Errorf("restored events = %q", got)
	}
}

func TestBackupAndRestoreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"backup"}, {"restore"}, {"backup", "extra"}, {"restore", "--input", "x", "extra"}} {
		if r := run(t, nil, args...); r.code != 2 {
			t.Errorf("%v = %d, want usage error", args, r.code)
		}
	}
}

func TestRestoreRefusals(t *testing.T) {
	db := seeded(t, "original")
	backup := filepath.Join(privateDir(t), "b.db")
	if r := run(t, dbEnv(db), "backup", "--output", backup); r.code != 0 {
		t.Fatal(r.stderr)
	}

	release, err := store.LockDatabase(db)
	if err != nil {
		t.Fatal(err)
	}
	r := run(t, dbEnv(db), "restore", "--input", backup)
	release()
	if r.code != 1 || !strings.Contains(r.stderr, "stop it first") {
		t.Errorf("restore while running = %d %q", r.code, r.stderr)
	}

	corrupt := filepath.Join(privateDir(t), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run(t, dbEnv(db), "restore", "--input", corrupt); r.code != 1 {
		t.Errorf("restore corrupt = %d %q", r.code, r.stderr)
	}
	if got := events(t, db); got != "original" {
		t.Errorf("events after refusals = %q", got)
	}
}
