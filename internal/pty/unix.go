//go:build unix

package pty

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	creackpty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Unix starts each command in a new session whose controlling terminal is a
// fresh PTY, so the child leads its own process group.
type Unix struct {
	// EnvPassthrough names environment variables, beyond the default
	// allowlist, that children inherit.
	EnvPassthrough []string
}

// Start starts cmd attached to a new PTY of the given size.
func (u Unix) Start(cmd Command, size Size) (Process, error) {
	master, tty, err := creackpty.Open()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	defer func() { _ = tty.Close() }()
	if err := creackpty.Setsize(master, &creackpty.Winsize{Rows: size.Rows, Cols: size.Cols}); err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("size pty: %w", err)
	}

	child := exec.Command(cmd.Path, cmd.Args...)
	child.Env = cmd.Env
	if child.Env == nil {
		child.Env = ChildEnvironment(os.Environ(), u.EnvPassthrough)
	}
	child.Dir = cmd.Dir
	child.Stdin, child.Stdout, child.Stderr = tty, tty, tty
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := child.Start(); err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	pollable, err := pollableFile(master)
	if err != nil {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
		return nil, err
	}
	return &unixProcess{cmd: child, master: pollable, waitExited: waitExited}, nil
}

// pollableFile replaces f with a non-blocking duplicate registered with the
// runtime poller, so Close interrupts a blocked Read.
func pollableFile(f *os.File) (*os.File, error) {
	defer func() { _ = f.Close() }()
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate pty: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("configure pty: %w", err)
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}

type unixProcess struct {
	cmd    *exec.Cmd
	master *os.File
	// waitExited blocks until the leader exits without reaping it, or
	// returns errors.ErrUnsupported.
	waitExited func(pid int) error

	// signalMu orders group signals with reaping: once the leader is reaped
	// its pid, and so the group id, may belong to an unrelated process.
	signalMu sync.Mutex
	reaped   bool

	waitOnce   sync.Once
	waitStatus ExitStatus
	waitErr    error
	closeOnce  sync.Once
	closeErr   error
}

func (p *unixProcess) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *unixProcess) Write(b []byte) (int, error) { return p.master.Write(b) }
func (p *unixProcess) Pid() int                    { return p.cmd.Process.Pid }

func (p *unixProcess) Resize(size Size) error {
	raw, err := p.master.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: size.Rows, Col: size.Cols})
	}); err != nil {
		return err
	}
	return ioctlErr
}

// SignalGroup signals the process group led by the child. After the leader
// is reaped it does nothing.
func (p *unixProcess) SignalGroup(sig syscall.Signal) error {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.reaped {
		return nil
	}
	return p.signalGroupLocked(sig)
}

func (p *unixProcess) signalGroupLocked(sig syscall.Signal) error {
	pid := p.cmd.Process.Pid
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.EPERM) {
		// macOS reports EPERM for a group whose only members are zombies.
		err = syscall.Kill(pid, sig)
	}
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Wait waits for the leader to exit, kills any remaining members of its
// group while the unreaped leader still pins the group id, then reaps it.
func (p *unixProcess) Wait() (ExitStatus, error) {
	p.waitOnce.Do(func() {
		pid := p.cmd.Process.Pid
		var status syscall.WaitStatus
		if p.waitExited(pid) == nil {
			p.signalMu.Lock()
			_ = p.signalGroupLocked(syscall.SIGKILL)
			err := p.cmd.Wait()
			p.reaped = true
			p.signalMu.Unlock()
			var exitErr *exec.ExitError
			if err != nil && !errors.As(err, &exitErr) {
				p.waitErr = err
				return
			}
			status, _ = p.cmd.ProcessState.Sys().(syscall.WaitStatus)
		} else if status, p.waitErr = p.pollReap(pid); p.waitErr != nil {
			return
		}
		if status.Signaled() {
			p.waitStatus = ExitStatus{Code: -1, Signal: unix.SignalName(status.Signal())}
		} else {
			p.waitStatus = ExitStatus{Code: status.ExitStatus()}
		}
	})
	return p.waitStatus, p.waitErr
}

// reapPollInterval is how often pollReap checks for the leader's exit.
const reapPollInterval = 20 * time.Millisecond

// pollReap reaps the leader with non-blocking waits, each under signalMu,
// so no group signal is sent between the reap and marking it reaped.
func (p *unixProcess) pollReap(pid int) (syscall.WaitStatus, error) {
	for {
		var status syscall.WaitStatus
		p.signalMu.Lock()
		got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if got == pid || (err != nil && !errors.Is(err, syscall.EINTR)) {
			p.reaped = true
			p.signalMu.Unlock()
			if err != nil {
				return 0, fmt.Errorf("wait: %w", err)
			}
			return status, nil
		}
		p.signalMu.Unlock()
		time.Sleep(reapPollInterval)
	}
}

func (p *unixProcess) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.master.Close() })
	return p.closeErr
}
