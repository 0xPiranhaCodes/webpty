//go:build !unix

package store

import "errors"

// LockDatabase is unavailable where webpty does not run natively.
func LockDatabase(string) (func() error, error) {
	return nil, errors.New("lock database: unsupported platform")
}

// LockState describes a database lock file.
type LockState struct {
	Exists, InUse, Writable bool
	Owner                   int
}

// InspectLock is unavailable where webpty does not run natively.
func InspectLock(string) (LockState, error) {
	return LockState{}, errors.New("inspect lock: unsupported platform")
}
