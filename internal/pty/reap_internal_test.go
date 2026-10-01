//go:build unix

package pty

import (
	"errors"
	"io"
	"syscall"
	"testing"
	"time"
)

// Without a way to observe exit without reaping, the leader must be reaped
// under signalMu so a group signal can never reach a recycled pid.
func TestPollingReapHoldsTheSignalLock(t *testing.T) {
	proc, err := Unix{}.Start(Command{Path: "/bin/sh", Args: []string{"-c", "exit 7"}}, Size{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	p := proc.(*unixProcess)
	p.waitExited = func(int) error { return errors.ErrUnsupported }
	go func() { _, _ = io.Copy(io.Discard, p) }()
	defer p.Close()

	p.signalMu.Lock()
	result := make(chan ExitStatus, 1)
	go func() {
		status, err := p.Wait()
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		result <- status
	}()
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(p.Pid(), 0); errors.Is(err, syscall.ESRCH) {
		p.signalMu.Unlock()
		t.Fatal("the leader was reaped while a group signal held signalMu")
	}
	p.signalMu.Unlock()

	select {
	case status := <-result:
		if status.Code != 7 {
			t.Fatalf("status = %+v, want exit 7", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return")
	}
	if err := p.SignalGroup(syscall.SIGTERM); err != nil {
		t.Fatalf("SignalGroup after reap = %v", err)
	}
}
