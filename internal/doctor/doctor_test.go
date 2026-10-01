package doctor_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/0xPiranhaCodes/webpty/internal/config"
	"github.com/0xPiranhaCodes/webpty/internal/doctor"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

var assets = fstest.MapFS{
	"index.html":            {Data: []byte(`<script type="module" src="/assets/index-abc.js"></script><link rel="stylesheet" href="/assets/index-abc.css">`)},
	"assets/index-abc.js":   {Data: []byte("console.log(1)")},
	"assets/index-abc.css":  {Data: []byte("body{}")},
	"assets/font-abc.woff2": {Data: []byte("font")},
}

func baseConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(key string) (string, bool) {
		if key == "WEBPTY_ADDRESS" {
			return "127.0.0.1:0", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.DatabasePath = filepath.Join(dir, "webpty.db")
	return cfg
}

func options(cfg config.Config) doctor.Options {
	return doctor.Options{Config: cfg, GOOS: "darwin", GOARCH: "arm64", Assets: assets}
}

func check(t *testing.T, report doctor.Report, name string) doctor.Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, report.Checks)
	return doctor.Check{}
}

func TestHealthyLocalSetupPasses(t *testing.T) {
	report := doctor.Run(context.Background(), options(baseConfig(t)))
	if report.Failed() {
		t.Fatalf("report failed:\n%s", report)
	}
	for _, name := range []string{"platform", "database directory", "database", "database lock", "command", "network", "frontend assets", "migrations"} {
		if c := check(t, report, name); c.Status != doctor.OK {
			t.Errorf("%s = %v %q, want OK", name, c.Status, c.Detail)
		}
	}
	if !strings.Contains(check(t, report, "database").Detail, "created") {
		t.Errorf("missing database detail = %q, want a first-start note", check(t, report, "database").Detail)
	}
}

func TestUnsupportedPlatformsFail(t *testing.T) {
	for _, platform := range [][2]string{{"windows", "amd64"}, {"linux", "386"}, {"freebsd", "amd64"}} {
		opts := options(baseConfig(t))
		opts.GOOS, opts.GOARCH = platform[0], platform[1]
		if c := check(t, doctor.Run(context.Background(), opts), "platform"); c.Status != doctor.Fail {
			t.Errorf("%s/%s = %v, want Fail", platform[0], platform[1], c.Status)
		}
	}
}

func TestExistingDatabaseReportsSchemaAndPermissions(t *testing.T) {
	cfg := baseConfig(t)
	s, err := store.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(cfg.DatabasePath, 0o600); err != nil {
		t.Fatal(err)
	}
	c := check(t, doctor.Run(context.Background(), options(cfg)), "database")
	if c.Status != doctor.OK || !strings.Contains(c.Detail, "schema version") {
		t.Fatalf("database = %v %q", c.Status, c.Detail)
	}

	if err := os.Chmod(cfg.DatabasePath, 0o644); err != nil {
		t.Fatal(err)
	}
	c = check(t, doctor.Run(context.Background(), options(cfg)), "database")
	if c.Status != doctor.Warn || !strings.Contains(c.Detail, "chmod 600") {
		t.Fatalf("readable database = %v %q, want a chmod warning", c.Status, c.Detail)
	}
}

func initDB(t *testing.T, path string) {
	t.Helper()
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Doctor may run as root against a service's database; a lock file it
// created would then be root-owned and stop the service from starting.
func TestDoctorCreatesNoLockFile(t *testing.T) {
	cfg := baseConfig(t)
	initDB(t, cfg.DatabasePath)
	before := dirEntries(t, filepath.Dir(cfg.DatabasePath))
	report := doctor.Run(context.Background(), options(cfg))
	if c := check(t, report, "database lock"); c.Status != doctor.OK || !strings.Contains(c.Detail, "not running") {
		t.Errorf("database lock = %v %q", c.Status, c.Detail)
	}
	if after := dirEntries(t, filepath.Dir(cfg.DatabasePath)); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("doctor changed the database directory: %v -> %v", before, after)
	}
}

func TestDoctorReportsARunningServerWithoutTouchingItsLock(t *testing.T) {
	cfg := baseConfig(t)
	initDB(t, cfg.DatabasePath)
	release, err := store.LockDatabase(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before, _ := os.Stat(cfg.DatabasePath + ".lock")
	c := check(t, doctor.Run(context.Background(), options(cfg)), "database lock")
	if c.Status != doctor.OK || !strings.Contains(c.Detail, "running webpty") {
		t.Errorf("database lock = %v %q", c.Status, c.Detail)
	}
	if after, err := os.Stat(cfg.DatabasePath + ".lock"); err != nil || !os.SameFile(before, after) {
		t.Errorf("the lock file was replaced: %v", err)
	}
	if _, err := store.LockDatabase(cfg.DatabasePath); err == nil {
		t.Error("doctor released the running server's lock")
	}
}

func TestDoctorExplainsALockThisUserCannotTake(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	cfg := baseConfig(t)
	initDB(t, cfg.DatabasePath)
	lock := cfg.DatabasePath + ".lock"
	if err := os.WriteFile(lock, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	c := check(t, doctor.Run(context.Background(), options(cfg)), "database lock")
	if c.Status != doctor.Fail || !strings.Contains(c.Detail, "not writable") || !strings.Contains(c.Detail, lock) {
		t.Errorf("read-only lock = %v %q", c.Status, c.Detail)
	}
	if err := os.Chmod(lock, 0); err != nil {
		t.Fatal(err)
	}
	c = check(t, doctor.Run(context.Background(), options(cfg)), "database lock")
	if c.Status != doctor.Fail || !strings.Contains(c.Detail, "permission denied") || !strings.Contains(c.Detail, "service user") {
		t.Errorf("unreadable lock = %v %q", c.Status, c.Detail)
	}
}

func TestDoctorRefusesASymlinkedLock(t *testing.T) {
	cfg := baseConfig(t)
	initDB(t, cfg.DatabasePath)
	if err := os.Symlink("/etc/hosts", cfg.DatabasePath+".lock"); err != nil {
		t.Fatal(err)
	}
	if c := check(t, doctor.Run(context.Background(), options(cfg)), "database lock"); c.Status != doctor.Fail {
		t.Errorf("symlinked lock = %v %q, want Fail", c.Status, c.Detail)
	}
}

func TestDatabaseProblemsFail(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, path string){
		"corrupt": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a database at all, just text"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"newer schema": func(t *testing.T, path string) {
			s, err := store.Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.DB().Exec(`INSERT INTO schema_migrations (version, name, applied_at, checksum) VALUES (?, 'x', 0, 'x')`,
				store.LatestSchemaVersion()+1); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig(t)
			damage(t, cfg.DatabasePath)
			report := doctor.Run(context.Background(), options(cfg))
			if c := check(t, report, "database"); c.Status != doctor.Fail {
				t.Fatalf("database = %v %q, want Fail", c.Status, c.Detail)
			}
			if !report.Failed() {
				t.Error("report did not fail")
			}
		})
	}
}

func TestUnsafeDatabaseDirectoriesFail(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DatabasePath = filepath.Join(t.TempDir(), "missing", "webpty.db")
	if c := check(t, doctor.Run(context.Background(), options(cfg)), "database directory"); c.Status != doctor.Fail {
		t.Errorf("missing directory = %v %q, want Fail", c.Status, c.Detail)
	}

	cfg = baseConfig(t)
	dir := filepath.Dir(cfg.DatabasePath)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if c := check(t, doctor.Run(context.Background(), options(cfg)), "database directory"); c.Status != doctor.Fail {
		t.Errorf("world-writable directory = %v %q, want Fail", c.Status, c.Detail)
	}

	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if c := check(t, doctor.Run(context.Background(), options(cfg)), "database directory"); c.Status != doctor.Fail {
			t.Errorf("read-only directory = %v %q, want Fail", c.Status, c.Detail)
		}
	}
}

func TestCommandProblemsFail(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Command = "/definitely/not/here"
	if c := check(t, doctor.Run(context.Background(), options(cfg)), "command"); c.Status != doctor.Fail {
		t.Errorf("missing command = %v %q, want Fail", c.Status, c.Detail)
	}
	cfg.Command = "sh"
	if c := check(t, doctor.Run(context.Background(), options(cfg)), "command"); c.Status != doctor.OK {
		t.Errorf("command on PATH = %v %q, want OK", c.Status, c.Detail)
	}
	cfg.Command = "/bin/sh"
	cfg.CommandDeny = []string{"/bin/sh"}
	if c := check(t, doctor.Run(context.Background(), options(cfg)), "command"); c.Status != doctor.Fail {
		t.Errorf("denied command = %v %q, want Fail", c.Status, c.Detail)
	}
}

func TestNetworkSecurityCompatibility(t *testing.T) {
	for name, tc := range map[string]struct {
		address, origin string
		secure          bool
		want            doctor.Status
	}{
		"loopback":                    {"127.0.0.1:0", "", false, doctor.OK},
		"https behind proxy":          {"0.0.0.0:0", "https://pty.example.com", true, doctor.OK},
		"remote without tls":          {"0.0.0.0:0", "http://pty.example.com", false, doctor.Warn},
		"https without secure cookie": {"0.0.0.0:0", "https://pty.example.com", false, doctor.Warn},
		"secure cookies over http":    {"0.0.0.0:0", "http://pty.example.com", true, doctor.Fail},
		"remote without origin":       {"0.0.0.0:0", "", false, doctor.Fail},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig(t)
			cfg.Address, cfg.PublicOrigin, cfg.SecureCookies = tc.address, tc.origin, tc.secure
			if c := check(t, doctor.Run(context.Background(), options(cfg)), "network"); c.Status != tc.want {
				t.Errorf("network = %v %q, want %v", c.Status, c.Detail, tc.want)
			}
		})
	}
}

func TestAddressInUseWarns(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := baseConfig(t)
	cfg.Address = busy.Addr().String()
	if c := check(t, doctor.Run(context.Background(), options(cfg)), "network"); c.Status != doctor.Warn || !strings.Contains(c.Detail, "in use") {
		t.Errorf("busy address = %v %q, want an in-use warning", c.Status, c.Detail)
	}
}

func TestMissingFrontendAssetsFail(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"no build":      {".gitkeep": {}},
		"missing chunk": {"index.html": assets["index.html"], "assets/index-abc.css": assets["assets/index-abc.css"]},
	} {
		opts := options(baseConfig(t))
		opts.Assets = fsys
		if c := check(t, doctor.Run(context.Background(), opts), "frontend assets"); c.Status != doctor.Fail {
			t.Errorf("%s = %v %q, want Fail", name, c.Status, c.Detail)
		}
	}
}

func TestReportNeverContainsSecrets(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret-value-123")
	cfg := baseConfig(t)
	cfg.CommandArgs = []string{"--token", "argument-secret-456"}
	cfg.ChildEnvPassthrough = []string{"AWS_SECRET_ACCESS_KEY"}
	text := doctor.Run(context.Background(), options(cfg)).String()
	for _, secret := range []string{"env-secret-value-123", "argument-secret-456", "CHANGEME"} {
		if strings.Contains(text, secret) {
			t.Errorf("report contains %q:\n%s", secret, text)
		}
	}
	if !strings.Contains(text, "2 arguments") {
		t.Errorf("report should count command arguments without showing them:\n%s", text)
	}
}
