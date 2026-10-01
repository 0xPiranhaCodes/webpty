// Package doctor checks whether a webpty configuration can run safely on
// this host. Its report names settings, paths, and counts but never prints
// passwords, tokens, command arguments, or environment values.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/0xPiranhaCodes/webpty/internal/config"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// Status is a check's outcome.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) String() string {
	switch s {
	case OK:
		return " OK "
	case Warn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// Check is one finding.
type Check struct {
	Name   string
	Status Status
	Detail string
}

// Report lists every check in a fixed order.
type Report struct {
	Checks []Check
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

func (r Report) String() string {
	var b strings.Builder
	failures, warnings := 0, 0
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "[%s] %s: %s\n", c.Status, c.Name, c.Detail)
		switch c.Status {
		case Fail:
			failures++
		case Warn:
			warnings++
		}
	}
	fmt.Fprintf(&b, "%d failed, %d warnings, %d checks\n", failures, warnings, len(r.Checks))
	return b.String()
}

// Options is what Run checks.
type Options struct {
	Config config.Config
	// GOOS and GOARCH name the platform being checked, normally runtime's.
	GOOS, GOARCH string
	// Assets is the embedded web UI.
	Assets fs.FS
}

// Run performs every check. It does not create or modify the database or
// its lock file; the directory check creates and removes one probe file.
func Run(ctx context.Context, opts Options) Report {
	cfg := opts.Config
	return Report{Checks: []Check{
		checkPlatform(opts.GOOS, opts.GOARCH),
		checkDatabaseDirectory(cfg.DatabasePath),
		checkDatabase(ctx, cfg.DatabasePath),
		checkLock(cfg.DatabasePath),
		checkMigrations(),
		checkCommand(cfg),
		checkNetwork(cfg),
		checkAssets(opts.Assets),
	}}
}

func checkPlatform(goos, goarch string) Check {
	c := Check{Name: "platform"}
	supportedOS := goos == "darwin" || goos == "linux"
	supportedArch := goarch == "amd64" || goarch == "arm64"
	if supportedOS && supportedArch {
		c.Detail = goos + "/" + goarch + " is supported"
		return c
	}
	c.Status = Fail
	c.Detail = goos + "/" + goarch + " is not supported; webpty runs on macOS and Linux, amd64 and arm64"
	if goos == "windows" {
		c.Detail += " (on Windows, use WSL or Docker)"
	}
	return c
}

func checkDatabaseDirectory(dbPath string) Check {
	c := Check{Name: "database directory"}
	dir := filepath.Dir(dbPath)
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail(c, "%s does not exist; create it with mode 700", dir)
	case err != nil:
		return fail(c, "%s: %v", dir, describe(err))
	case !info.IsDir():
		return fail(c, "%s is not a directory", dir)
	}
	mode := info.Mode()
	if mode.Perm()&0o002 != 0 && mode&fs.ModeSticky == 0 {
		return fail(c, "%s is writable by every user, who could replace the database; chmod 700 it", dir)
	}
	probe, err := os.CreateTemp(dir, ".webpty-doctor-*")
	if err != nil {
		return fail(c, "%s is not writable: %v", dir, describe(err))
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	switch {
	case mode.Perm()&0o002 != 0:
		c.Status, c.Detail = Warn, dir+" is shared by every user; prefer a private directory (mode 700)"
	case mode.Perm()&0o020 != 0:
		c.Status, c.Detail = Warn, dir+" is writable by its group; prefer mode 700"
	default:
		c.Detail = dir + " is writable and not writable by other users"
	}
	return c
}

func checkDatabase(ctx context.Context, dbPath string) Check {
	c := Check{Name: "database"}
	info, err := os.Lstat(dbPath)
	if errors.Is(err, fs.ErrNotExist) {
		c.Detail = dbPath + " does not exist yet; it will be created on first start"
		return c
	}
	if err != nil {
		return fail(c, "%s: %v", dbPath, describe(err))
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		if info, err = os.Stat(dbPath); err != nil {
			return fail(c, "%s: %v", dbPath, describe(err))
		}
	}
	inspection, err := store.Inspect(ctx, dbPath)
	if err != nil {
		return fail(c, "%s: %v", dbPath, err)
	}
	c.Detail = fmt.Sprintf("%s: integrity ok, schema version %d", dbPath, inspection.SchemaVersion)
	if inspection.Pending > 0 {
		c.Detail += fmt.Sprintf(", %d migrations will be applied on next start", inspection.Pending)
	}
	if info.Mode().Perm()&0o077 != 0 {
		c.Status = Warn
		c.Detail += fmt.Sprintf("; mode %o lets other users read password and session hashes, chmod 600 it", info.Mode().Perm())
	}
	return c
}

// checkLock never creates the lock file: doctor often runs as root or as
// an administrator, and a lock file it created would belong to that user
// and stop the service account from starting webpty. When the file is
// missing, checkDatabaseDirectory has already shown that it can be created.
func checkLock(dbPath string) Check {
	c := Check{Name: "database lock"}
	name := dbPath + ".lock"
	state, err := store.InspectLock(dbPath)
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fail(c, "%s: permission denied; it belongs to another user (owner of a running service?). Run doctor as the service user, or fix its owner with chown", name)
	case err != nil:
		return fail(c, "%v", err)
	case !state.Exists:
		c.Detail = "no lock file; webpty is not running and will create " + name
	case state.InUse:
		c.Detail = "the lock is held by a running webpty (" + name + ")"
	case !state.Writable:
		return fail(c, "%s is owned by uid %d and not writable by uid %d, so webpty serve as this user cannot lock the database; run doctor as the service user, or chown the file", name, state.Owner, os.Getuid())
	default:
		c.Detail = name + " is free; webpty is not running"
	}
	return c
}

func checkMigrations() Check {
	c := Check{Name: "migrations"}
	n, err := store.EmbeddedMigrations()
	if err != nil {
		return fail(c, "embedded migrations are invalid: %v", err)
	}
	c.Detail = fmt.Sprintf("%d embedded migrations, ordered and checksummed", n)
	return c
}

func checkCommand(cfg config.Config) Check {
	c := Check{Name: "command"}
	resolved := cfg.Command
	if !strings.Contains(cfg.Command, "/") {
		found, err := exec.LookPath(cfg.Command)
		if err != nil {
			return fail(c, "%s is not on PATH", cfg.Command)
		}
		resolved = found
	}
	info, err := os.Stat(resolved)
	switch {
	case err != nil:
		return fail(c, "%s: %v", resolved, describe(err))
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0:
		return fail(c, "%s is not an executable file", resolved)
	}
	policy, err := session.NewCommandPolicy(cfg.CommandAllow, cfg.CommandDeny)
	if err != nil {
		return fail(c, "command policy: %v", err)
	}
	if !policy.Permits(cfg.Command) {
		return fail(c, "%s is not permitted by WEBPTY_COMMAND_ALLOW/WEBPTY_COMMAND_DENY", cfg.Command)
	}
	arguments := "no arguments"
	switch n := len(cfg.CommandArgs); n {
	case 0:
	case 1:
		arguments = "1 argument"
	default:
		arguments = fmt.Sprintf("%d arguments", n)
	}
	rule := "no allow or deny list"
	if len(cfg.CommandAllow) > 0 || len(cfg.CommandDeny) > 0 {
		rule = fmt.Sprintf("permitted by a policy of %d allowed and %d denied paths", len(cfg.CommandAllow), len(cfg.CommandDeny))
	}
	c.Detail = fmt.Sprintf("%s (%s) with %s; %s; %d extra environment variables passed to terminals",
		cfg.Command, resolved, arguments, rule, len(cfg.ChildEnvPassthrough))
	return c
}

func checkNetwork(cfg config.Config) Check {
	c := Check{Name: "network"}
	if _, _, err := net.SplitHostPort(cfg.Address); err != nil {
		return fail(c, "WEBPTY_ADDRESS %q: %v", cfg.Address, err)
	}
	loopback := config.IsLoopbackAddress(cfg.Address)
	var origin *url.URL
	if cfg.PublicOrigin != "" {
		parsed, err := url.Parse(cfg.PublicOrigin)
		if err != nil {
			return fail(c, "WEBPTY_PUBLIC_ORIGIN is not a URL")
		}
		origin = parsed
	}
	switch {
	case !loopback && origin == nil:
		return fail(c, "%s is reachable from other hosts but WEBPTY_PUBLIC_ORIGIN is not set", cfg.Address)
	case cfg.SecureCookies && origin != nil && origin.Scheme != "https":
		return fail(c, "secure cookies are on but %s is not https, so browsers would drop the session cookie", cfg.PublicOrigin)
	}
	if listener, err := net.Listen("tcp", cfg.Address); err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			c.Status, c.Detail = Warn, cfg.Address+" is already in use (is webpty already running?)"
			return c
		}
		return fail(c, "cannot listen on %s: %v", cfg.Address, describe(err))
	} else {
		_ = listener.Close()
	}
	switch {
	case loopback && origin == nil:
		c.Detail = cfg.Address + " accepts connections from this machine only"
	case origin != nil && origin.Scheme != "https" && !loopback:
		c.Status = Warn
		c.Detail = fmt.Sprintf("%s is reachable from other hosts over plain HTTP (%s); terminate TLS at a reverse proxy", cfg.Address, cfg.PublicOrigin)
	case origin != nil && origin.Scheme == "https" && !cfg.SecureCookies:
		c.Status = Warn
		c.Detail = cfg.PublicOrigin + " is https but WEBPTY_SECURE_COOKIES=false; session cookies could leak over plain HTTP"
	default:
		c.Detail = fmt.Sprintf("%s serves %s; secure cookies %t", cfg.Address, cfg.PublicOrigin, cfg.SecureCookies)
	}
	return c
}

var assetReference = regexp.MustCompile(`(?:src|href)="/(assets/[^"?#]+)"`)

func checkAssets(assets fs.FS) Check {
	c := Check{Name: "frontend assets"}
	if assets == nil {
		return fail(c, "no web UI is embedded")
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return fail(c, "the web UI is not embedded in this build; build the frontend before the binary (make build)")
	}
	refs := assetReference.FindAllSubmatch(index, -1)
	if len(refs) == 0 {
		return fail(c, "index.html references no bundled assets")
	}
	for _, ref := range refs {
		name := path.Clean(string(ref[1]))
		if _, err := fs.Stat(assets, name); err != nil {
			return fail(c, "index.html references %s, which is not embedded", name)
		}
	}
	c.Detail = fmt.Sprintf("embedded index.html and its %d bundled assets", len(refs))
	return c
}

func fail(c Check, format string, args ...any) Check {
	c.Status = Fail
	c.Detail = fmt.Sprintf(format, args...)
	return c
}

// describe drops the path an *fs.PathError repeats.
func describe(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
