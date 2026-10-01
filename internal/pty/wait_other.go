//go:build unix && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package pty

import "errors"

// waitExited is unavailable, so leftover group members are not killed when
// the leader exits on its own, and Wait reaps by polling instead.
func waitExited(int) error { return errors.ErrUnsupported }
