// Package pty starts local child processes attached to pseudo-terminals.
package pty

import (
	"strings"
	"syscall"
)

// Command is an executable and its argument vector. It is never passed to a
// shell parser.
type Command struct {
	Path string
	Args []string
	// Env replaces the child environment when non-nil; otherwise the child
	// receives ChildEnvironment(os.Environ(), the starter's passthrough).
	Env []string
	Dir string
}

// Size is a terminal size in character cells.
type Size struct {
	Rows, Cols uint16
}

// ExitStatus describes how a process ended. Code is -1 when the process was
// killed by Signal.
type ExitStatus struct {
	Code   int
	Signal string
}

// Process is one child process and the PTY master it owns.
type Process interface {
	// Read reads PTY output. It returns an error once the PTY is closed or
	// every holder of the terminal has exited.
	Read(p []byte) (int, error)
	// Write writes terminal input.
	Write(p []byte) (int, error)
	Resize(Size) error
	// SignalGroup signals the child's process group. Signalling a group that
	// no longer exists is not an error.
	SignalGroup(syscall.Signal) error
	// Wait reaps the child. It may be called more than once.
	Wait() (ExitStatus, error)
	// Close releases the PTY master and unblocks Read. It is idempotent.
	Close() error
	Pid() int
}

// Starter creates processes.
type Starter interface {
	Start(Command, Size) (Process, error)
}

// childEnvironmentAllowlist names the variables a child inherits by
// default: identity, locale, time zone, and search paths. Anything else,
// such as cloud credentials or tokens in webpty's own environment, must be
// named explicitly in a passthrough list.
var childEnvironmentAllowlist = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "PATH": true,
	"LANG": true, "TZ": true, "TMPDIR": true,
}

// ChildEnvironment returns the allowlisted entries of environ, plus those
// named in passthrough, with TERM set for an xterm-compatible client.
// LC_* locale variables are allowlisted. WEBPTY_* variables are never
// passed, even when named.
func ChildEnvironment(environ, passthrough []string) []string {
	extra := make(map[string]bool, len(passthrough))
	for _, name := range passthrough {
		extra[name] = true
	}
	env := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(name, "WEBPTY_") || name == "TERM" {
			continue
		}
		if childEnvironmentAllowlist[name] || strings.HasPrefix(name, "LC_") || extra[name] {
			env = append(env, entry)
		}
	}
	return append(env, "TERM=xterm-256color")
}
