// Package ptytest provides an in-memory pty.Starter for deterministic tests.
package ptytest

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
)

// Starter records started processes. Set Err to make the next Start fail.
type Starter struct {
	mu      sync.Mutex
	err     error
	nextPid int
	started chan *Process
}

// NewStarter returns an empty Starter.
func NewStarter() *Starter {
	return &Starter{nextPid: 1000, started: make(chan *Process, 1024)}
}

// FailNext makes the next Start return err.
func (s *Starter) FailNext(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Start implements pty.Starter.
func (s *Starter) Start(cmd pty.Command, size pty.Size) (pty.Process, error) {
	s.mu.Lock()
	if err := s.err; err != nil {
		s.err = nil
		s.mu.Unlock()
		return nil, err
	}
	s.nextPid++
	p := &Process{
		Command: cmd,
		pid:     s.nextPid,
		sizes:   []pty.Size{size},
		output:  make(chan []byte, 1024),
		exited:  make(chan struct{}),
		closed:  make(chan struct{}),
	}
	s.mu.Unlock()
	s.started <- p
	return p, nil
}

// Next returns the next started process, or nil after timeout.
func (s *Starter) Next(timeout time.Duration) *Process {
	select {
	case p := <-s.started:
		return p
	case <-time.After(timeout):
		return nil
	}
}

// Process is a scripted pty.Process. SIGTERM and SIGKILL end it unless
// IgnoreTerm is set, in which case only SIGKILL does.
type Process struct {
	Command pty.Command
	pid     int

	mu         sync.Mutex
	ignoreTerm bool
	input      bytes.Buffer
	sizes      []pty.Size
	signals    []syscall.Signal
	status     pty.ExitStatus
	waitErr    error
	blockWrite chan struct{}
	// blockedWriters counts Write calls parked by BlockWrites.
	blockedWriters int

	output    chan []byte
	pending   []byte
	exitOnce  sync.Once
	exited    chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

// IgnoreTerm makes SIGTERM have no effect.
func (p *Process) IgnoreTerm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ignoreTerm = true
}

// BlockWrites makes Write block until the returned function is called.
func (p *Process) BlockWrites() (release func()) {
	ch := make(chan struct{})
	p.mu.Lock()
	p.blockWrite = ch
	p.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// BlockedWriters reports how many Write calls are parked by BlockWrites.
func (p *Process) BlockedWriters() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blockedWriters
}

// Emit queues terminal output.
func (p *Process) Emit(data string) { p.output <- []byte(data) }

// Exit ends the process with status.
func (p *Process) Exit(status pty.ExitStatus) { p.finish(status, nil) }

// FailWait ends the process with a Wait error.
func (p *Process) FailWait(err error) { p.finish(pty.ExitStatus{}, err) }

func (p *Process) finish(status pty.ExitStatus, err error) {
	p.exitOnce.Do(func() {
		p.mu.Lock()
		p.status, p.waitErr = status, err
		p.mu.Unlock()
		close(p.exited)
	})
}

// Input returns everything written to the process.
func (p *Process) Input() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.String()
}

// Sizes returns the initial size followed by every resize.
func (p *Process) Sizes() []pty.Size {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pty.Size(nil), p.sizes...)
}

// Signals returns every signal sent to the process group.
func (p *Process) Signals() []syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]syscall.Signal(nil), p.signals...)
}

// Closed reports whether Close was called.
func (p *Process) Closed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// Exited reports whether the process has ended.
func (p *Process) Exited() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

func (p *Process) Pid() int { return p.pid }

// Read returns queued output. After exit it drains queued output and then
// reports EOF, like a PTY whose last holder has gone.
func (p *Process) Read(b []byte) (int, error) {
	if len(p.pending) == 0 {
		select {
		case data := <-p.output:
			p.pending = data
		case <-p.closed:
			return 0, os.ErrClosed
		case <-p.exited:
			select {
			case data := <-p.output:
				p.pending = data
			default:
				return 0, io.EOF
			}
		}
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *Process) Write(b []byte) (int, error) {
	p.mu.Lock()
	block := p.blockWrite
	if block != nil {
		p.blockedWriters++
	}
	p.mu.Unlock()
	if block != nil {
		defer func() {
			p.mu.Lock()
			p.blockedWriters--
			p.mu.Unlock()
		}()
		select {
		case <-block:
		case <-p.closed:
			return 0, os.ErrClosed
		}
	}
	if p.Closed() {
		return 0, os.ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input.Write(b)
}

func (p *Process) Resize(size pty.Size) error {
	if p.Closed() {
		return os.ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sizes = append(p.sizes, size)
	return nil
}

func (p *Process) SignalGroup(sig syscall.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	ignore := p.ignoreTerm
	p.mu.Unlock()
	switch {
	case sig == syscall.SIGKILL:
		p.finish(pty.ExitStatus{Code: -1, Signal: "SIGKILL"}, nil)
	case sig == syscall.SIGTERM && !ignore:
		p.finish(pty.ExitStatus{Code: -1, Signal: "SIGTERM"}, nil)
	}
	return nil
}

func (p *Process) Wait() (pty.ExitStatus, error) {
	<-p.exited
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, p.waitErr
}

func (p *Process) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

// ErrStart is a convenient start failure.
var ErrStart = errors.New("ptytest: start failed")
