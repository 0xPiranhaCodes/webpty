//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package pty

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitExited blocks until pid has exited, leaving it unreaped. The BSDs'
// wait4 does not honor WNOWAIT, so exit is observed through kqueue instead.
func waitExited(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	changes := make([]unix.Kevent_t, 1)
	unix.SetKevent(&changes[0], pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	changes[0].Fflags = unix.NOTE_EXIT
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, changes, events, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			changes = nil
			continue
		case errors.Is(err, unix.ESRCH):
			// The child already exited; only this process reaps it.
			return nil
		case err != nil:
			return err
		case n == 1 && events[0].Flags&unix.EV_ERROR != 0:
			if unix.Errno(events[0].Data) == unix.ESRCH {
				return nil
			}
			return unix.Errno(events[0].Data)
		case n == 1:
			return nil
		}
		changes = nil
	}
}
