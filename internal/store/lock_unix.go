//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// LockDatabase takes an exclusive advisory lock on path's companion
// path+".lock" file. The server holds it for its whole run and offline
// maintenance takes it first, so a restore never swaps a database that a
// running server has open. It fails with ErrDatabaseInUse instead of
// waiting, and refuses a lock path that is a symlink or not a regular file.
func LockDatabase(path string) (release func() error, err error) {
	name := path + ".lock"
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock database: %w", lockOpenError(name, err))
	}
	file := os.NewFile(uintptr(fd), name)
	if err := requireRegular(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock database: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", path, ErrDatabaseInUse)
		}
		return nil, fmt.Errorf("lock database: %w", err)
	}
	return func() error {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return file.Close()
	}, nil
}

// LockState describes path+".lock" as InspectLock found it.
type LockState struct {
	Exists bool
	// InUse means a process, normally a running server, holds the lock.
	InUse bool
	// Writable means this process could open the lock file to take it.
	Writable bool
	// Owner is the lock file's owning uid.
	Owner int
}

// InspectLock reports on path+".lock" without creating, replacing, or
// unlinking it. A free lock is probed with a momentary shared lock, which
// never blocks and is released before InspectLock returns.
func InspectLock(path string) (LockState, error) {
	name := path + ".lock"
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return LockState{}, nil
	}
	if err != nil {
		return LockState{}, lockOpenError(name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := requireRegular(file); err != nil {
		return LockState{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return LockState{}, &os.PathError{Op: "stat", Path: name, Err: err}
	}
	state := LockState{
		Exists:   true,
		Writable: unix.Access(name, unix.W_OK) == nil,
		Owner:    int(st.Uid),
	}
	switch err := unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB); {
	case errors.Is(err, unix.EWOULDBLOCK):
		state.InUse = true
	case err != nil:
		return LockState{}, &os.PathError{Op: "flock", Path: name, Err: err}
	default:
		_ = unix.Flock(fd, unix.LOCK_UN)
	}
	return state, nil
}

func lockOpenError(name string, err error) error {
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%s is a symbolic link; remove it", name)
	}
	return &os.PathError{Op: "open", Path: name, Err: err}
}

func requireRegular(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; remove it", file.Name())
	}
	return nil
}
