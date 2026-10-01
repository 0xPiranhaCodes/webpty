package pty

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitExited blocks until pid has exited, leaving it unreaped.
func waitExited(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
