//go:build !unix

package pty

import "errors"

// Unix is unavailable on this platform.
type Unix struct {
	EnvPassthrough []string
}

// Start always fails: PTY sessions require a Unix host.
func (Unix) Start(Command, Size) (Process, error) {
	return nil, errors.New("pty: unsupported platform")
}
