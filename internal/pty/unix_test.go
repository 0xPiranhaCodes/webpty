//go:build unix

package pty_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
)

// output accumulates everything read from a process.
type output struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func collect(p pty.Process) *output {
	o := &output{done: make(chan struct{})}
	go func() {
		defer close(o.done)
		chunk := make([]byte, 4096)
		for {
			n, err := p.Read(chunk)
			o.mu.Lock()
			o.buf.Write(chunk[:n])
			o.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *output) waitFor(t *testing.T, substring string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(o.String(), substring) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output %q never contained %q", o.String(), substring)
}

func start(t *testing.T, size pty.Size, path string, args ...string) pty.Process {
	t.Helper()
	p, err := pty.Unix{}.Start(pty.Command{Path: path, Args: args}, size)
	if err != nil {
		t.Fatalf("Start(%s): %v", path, err)
	}
	t.Cleanup(func() {
		_ = p.SignalGroup(syscall.SIGKILL)
		_ = p.Close()
	})
	return p
}

func wait(t *testing.T, p pty.Process) pty.ExitStatus {
	t.Helper()
	type result struct {
		status pty.ExitStatus
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := p.Wait()
		done <- result{status, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		return r.status
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit")
		return pty.ExitStatus{}
	}
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Children wait for a line of input before exiting: on macOS, output still
// buffered in the PTY when the child exits can be discarded before the
// parent reads it, so a command that exits at once races the reader.
func TestUnixProducesOutputAndExitCode(t *testing.T) {
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/bin/sh", "-c", "echo hello-pty; read -r _; exit 3")
	out := collect(p)
	out.waitFor(t, "hello-pty")
	if _, err := p.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	status := wait(t, p)
	if status.Code != 3 || status.Signal != "" {
		t.Fatalf("status = %+v, want code 3", status)
	}
}

func TestUnixDoesNotInterpretArgumentsWithAShell(t *testing.T) {
	// The arguments reach the script as positional parameters; a shell
	// wrapper around the command line would expand them first.
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/bin/sh", "-c", `printf '%s ' "$@"; read -r _`, "sh", "$HOME", "a;b", "`id`")
	out := collect(p)
	out.waitFor(t, "$HOME a;b `id` ")
	if _, err := p.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	wait(t, p)
}

func TestUnixStartFailsForMissingExecutable(t *testing.T) {
	_, err := pty.Unix{}.Start(pty.Command{Path: "/nonexistent/webpty-test-binary"}, pty.Size{Rows: 24, Cols: 80})
	if err == nil {
		t.Fatal("Start succeeded for a missing executable")
	}
}

func TestUnixAppliesInitialSizeAndResize(t *testing.T) {
	p := start(t, pty.Size{Rows: 30, Cols: 100}, "/bin/sh", "-c", "stty size; read line; stty size")
	out := collect(p)
	out.waitFor(t, "30 100")
	if err := p.Resize(pty.Size{Rows: 42, Cols: 132}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if _, err := p.Write([]byte("\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out.waitFor(t, "42 132")
	wait(t, p)
}

func TestUnixEchoesInput(t *testing.T) {
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/bin/cat")
	out := collect(p)
	if _, err := p.Write([]byte("typed-input\n")); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, "typed-input")
	if err := p.SignalGroup(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait(t, p)
}

func TestUnixSignalGroupTerminatesProcessAndDescendants(t *testing.T) {
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/bin/sh", "-c", "sleep 60 & echo child=$!; wait")
	out := collect(p)
	child := childPid(t, out)

	if err := p.SignalGroup(syscall.SIGTERM); err != nil {
		t.Fatalf("SignalGroup: %v", err)
	}
	status := wait(t, p)
	if status.Signal != "SIGTERM" || status.Code != -1 {
		t.Fatalf("status = %+v, want SIGTERM", status)
	}
	if alive(p.Pid()) {
		t.Errorf("leader %d still exists after Wait", p.Pid())
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(child) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(child) {
		t.Errorf("descendant %d survived group termination", child)
	}
}

func childPid(t *testing.T, out *output) int {
	t.Helper()
	out.waitFor(t, "\n")
	text := out.String()
	index := strings.Index(text, "child=")
	if index < 0 {
		t.Fatalf("output %q has no child pid", text)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(text[index+len("child="):], "\n", 2)[0]))
	if err != nil {
		t.Fatalf("parse child pid from %q: %v", text, err)
	}
	return pid
}

func TestUnixWaitKillsGroupMembersLeftByAnExitedLeader(t *testing.T) {
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/bin/sh", "-c", `trap "" HUP; sleep 60 & echo child=$!`)
	out := collect(p)
	child := childPid(t, out)
	status := wait(t, p)
	if status.Code != 0 {
		t.Fatalf("status = %+v, want 0", status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(child) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(child) {
		t.Fatalf("group member %d survived its leader", child)
	}
	select {
	case <-out.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Read still blocked after every terminal holder exited")
	}
}

func TestUnixKillAfterIgnoredTerm(t *testing.T) {
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/bin/sh", "-c", `trap "" TERM; echo ready; exec sleep 60`)
	out := collect(p)
	out.waitFor(t, "ready")
	time.Sleep(50 * time.Millisecond)
	if err := p.SignalGroup(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(exited)
	}()
	select {
	case <-exited:
		t.Fatal("process exited on an ignored SIGTERM")
	case <-time.After(200 * time.Millisecond):
	}
	if err := p.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("process survived SIGKILL")
	}
}

func TestUnixCloseUnblocksReadAndIsIdempotent(t *testing.T) {
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/bin/sleep", "60")
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(readerFunc(p.Read))
		readDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Read still blocked after Close")
	}
	if err := p.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wait(t, p)
	if err := p.SignalGroup(syscall.SIGKILL); err != nil {
		t.Fatalf("SignalGroup after reap = %v, want nil", err)
	}
}

func TestUnixChildDoesNotInheritServerSecrets(t *testing.T) {
	t.Setenv("WEBPTY_TEST_SECRET", "do-not-leak")
	p := start(t, pty.Size{Rows: 24, Cols: 80}, "/usr/bin/env")
	out := collect(p)
	wait(t, p)
	<-out.done
	text := out.String()
	if strings.Contains(text, "do-not-leak") {
		t.Errorf("child environment contains WEBPTY_ variable: %q", text)
	}
	if !strings.Contains(text, "TERM=xterm-256color") {
		t.Errorf("child environment lacks TERM=xterm-256color: %q", text)
	}
	if home := os.Getenv("HOME"); home != "" && !strings.Contains(text, "HOME="+home) {
		t.Errorf("child environment lacks HOME")
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
