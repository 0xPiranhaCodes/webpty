//go:build unix

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestInspectLockWithoutALockFileCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "webpty.db")
	state, err := InspectLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Exists || state.InUse {
		t.Errorf("state = %+v, want no lock", state)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("InspectLock left %v behind", entries)
	}
}

func TestInspectLockSeesAHeldLockWithoutReplacingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webpty.db")
	release, err := LockDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	state, err := InspectLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || !state.InUse || !state.Writable {
		t.Errorf("state = %+v, want an existing, writable lock in use", state)
	}
	after, err := os.Stat(path + ".lock")
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("the lock file was replaced: %v", err)
	}
	// The holder still owns the lock.
	if _, err := LockDatabase(path); !errors.Is(err, ErrDatabaseInUse) {
		t.Errorf("LockDatabase after inspection = %v, want ErrDatabaseInUse", err)
	}
}

func TestInspectLockOfAFreeLockLeavesItFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webpty.db")
	release, err := LockDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = release()
	state, err := InspectLock(path)
	if err != nil || !state.Exists || state.InUse {
		t.Fatalf("state = %+v, %v; want an existing free lock", state, err)
	}
	again, err := LockDatabase(path)
	if err != nil {
		t.Fatalf("LockDatabase after inspection: %v", err)
	}
	_ = again()
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
}

// A lock left by a server running as another user (root or a service
// account) is simulated by permissions this user lacks.
func TestInspectLockReportsALockThisUserCannotUse(t *testing.T) {
	skipIfRoot(t)
	path := filepath.Join(t.TempDir(), "webpty.db")
	if err := os.WriteFile(path+".lock", nil, 0o400); err != nil {
		t.Fatal(err)
	}
	state, err := InspectLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.Writable || state.Owner != os.Geteuid() {
		t.Errorf("read-only lock state = %+v", state)
	}

	if err := os.Chmod(path+".lock", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectLock(path); !errors.Is(err, os.ErrPermission) {
		t.Errorf("unreadable lock err = %v, want a permission error", err)
	}
	if info, err := os.Stat(path + ".lock"); err != nil || info.Mode().Perm() != 0 {
		t.Errorf("the lock file was changed: %v %v", info, err)
	}
}

func TestLocksRefuseASymlinkedOrSpecialLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "webpty.db")
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectLock(path); err == nil {
		t.Error("InspectLock followed a symlinked lock file")
	}
	if release, err := LockDatabase(path); err == nil {
		_ = release()
		t.Error("LockDatabase followed a symlinked lock file")
	}
	if got, _ := os.ReadFile(victim); !bytes.Equal(got, []byte("keep")) {
		t.Error("the symlink target was modified")
	}

	fifo := filepath.Join(t.TempDir(), "webpty.db")
	if err := syscall.Mkfifo(fifo+".lock", 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := InspectLock(fifo); err == nil {
		t.Error("InspectLock accepted a FIFO as the lock file")
	}
}

// Inspecting a stopped database must not leave -wal or -shm files: when
// doctor runs as root they would be root-owned and break the service.
func TestInspectOfAStoppedDatabaseCreatesNoFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "webpty.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, _ := os.ReadDir(dir)
	inspection, err := Inspect(ctx, path)
	if err != nil || inspection.SchemaVersion == 0 {
		t.Fatalf("Inspect = %+v, %v", inspection, err)
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Errorf("Inspect changed the directory: %v -> %v", before, after)
	}

	// A running server's write-ahead log is still read.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Inspect(ctx, path); err != nil {
		t.Fatalf("Inspect while open: %v", err)
	}
}

// Publishing a backup never replaces a file that appears at the
// destination while the backup is written, even where hard links fail.
func TestBackupNeverReplacesAFileThatAppearsDuringPublish(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "webpty.db")
	s, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	dst := filepath.Join(t.TempDir(), "backup.db")
	intruder := []byte("written by someone else")
	linkFile = func(oldname, newname string) error {
		if err := os.WriteFile(newname, intruder, 0o600); err != nil {
			t.Fatal(err)
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	t.Cleanup(func() { linkFile = os.Link })
	if _, err := Backup(ctx, src, dst, BackupOptions{}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Backup err = %v, want ErrExist", err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, intruder) {
		t.Error("the file that appeared at the destination was replaced")
	}

	// Without the race, a filesystem without hard links still gets a backup.
	dst2 := filepath.Join(t.TempDir(), "backup.db")
	linkFile = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	if _, err := Backup(ctx, src, dst2, BackupOptions{}); err != nil {
		t.Fatalf("Backup without hard links: %v", err)
	}
	if _, err := ReadBackupMetadata(ctx, dst2); err != nil {
		t.Fatalf("copied backup is not readable: %v", err)
	}
	if info, err := os.Stat(dst2); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("copied backup = %v, %v; want mode 600", info, err)
	}
}
