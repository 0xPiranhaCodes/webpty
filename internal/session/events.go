package session

import (
	"context"
	"io"
	"sync"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// EventOverhead is the fixed per-event cost charged against replay and
// client queue budgets, so floods of tiny chunks stay bounded.
const EventOverhead = 48

// EventKind distinguishes output, size changes, and the final exit event.
type EventKind int

const (
	EventOutput EventKind = iota + 1
	EventExit
	EventResize
)

// Event is one item delivered to a subscriber. Data must not be modified.
// Resize events carry Rows and Cols, have no Seq, and are not replayed: a
// new subscriber learns the size from Info.
type Event struct {
	Kind EventKind
	Seq  uint64
	Data []byte
	Exit Exit
	Rows int
	Cols int
}

// Exit describes how a session ended. Code is nil when the process was
// killed by Signal or failed.
type Exit struct {
	State  store.TerminalState
	Code   *int
	Signal string
}

func eventCost(e Event) int { return len(e.Data) + EventOverhead }

// replayBuffer keeps the most recent output events within a byte budget.
type replayBuffer struct {
	limit  int
	events []Event
	head   int
	bytes  int
}

func (b *replayBuffer) add(e Event) {
	b.events = append(b.events, e)
	b.bytes += eventCost(e)
	for b.bytes > b.limit && b.head < len(b.events) {
		b.bytes -= eventCost(b.events[b.head])
		b.events[b.head] = Event{}
		b.head++
	}
	if b.head > 64 && b.head*2 > len(b.events) {
		b.events = append([]Event(nil), b.events[b.head:]...)
		b.head = 0
	}
}

// after returns buffered events. When resume is set it returns only events
// after afterSeq, or a ReplayGapError if some of them were evicted.
func (b *replayBuffer) after(resume bool, afterSeq, lastSeq uint64) ([]Event, error) {
	live := b.events[b.head:]
	if !resume {
		return append([]Event(nil), live...), nil
	}
	first := lastSeq + 1
	if len(live) > 0 {
		first = live[0].Seq
	}
	if afterSeq > lastSeq || afterSeq+1 < first {
		return nil, &ReplayGapError{AfterSeq: afterSeq, FirstSeq: first, LastSeq: lastSeq}
	}
	skip := int(afterSeq + 1 - first)
	return append([]Event(nil), live[skip:]...), nil
}

// Subscription is one viewer's bounded event queue. Enqueueing never blocks:
// a viewer whose queue exceeds its byte budget is disconnected with
// ErrSlowConsumer. Next must be called from one goroutine at a time.
type Subscription struct {
	terminal *terminal
	info     Info
	lastSeq  uint64
	limit    int

	mu       sync.Mutex
	queue    []Event
	head     int
	bytes    int
	exit     *Exit
	exitSent bool
	err      error
	notify   chan struct{}

	closeOnce sync.Once
}

// Info is the session metadata at the time of subscription.
func (s *Subscription) Info() Info { return s.info }

// LastSeq is the newest output sequence at the time of subscription.
func (s *Subscription) LastSeq() uint64 { return s.lastSeq }

// Next returns the next output or resize event, then one exit event when the session
// ends, then io.EOF. It returns ErrSlowConsumer if the viewer fell behind and
// ErrSubscriptionClosed after Close.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return Event{}, err
		}
		if s.head < len(s.queue) {
			event := s.queue[s.head]
			s.queue[s.head] = Event{}
			s.head++
			s.bytes -= eventCost(event)
			if s.head == len(s.queue) {
				s.queue, s.head = s.queue[:0], 0
			}
			s.mu.Unlock()
			return event, nil
		}
		if s.exit != nil {
			sent := s.exitSent
			s.exitSent = true
			exit := *s.exit
			s.mu.Unlock()
			if sent {
				return Event{}, io.EOF
			}
			return Event{Kind: EventExit, Exit: exit}, nil
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// Close detaches the viewer. The session keeps running. It is idempotent.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.err == nil {
			s.err = ErrSubscriptionClosed
		}
		s.queue, s.head, s.bytes = nil, 0, 0
		s.wake()
		s.mu.Unlock()
		s.terminal.detach(s)
	})
}

// push enqueues e and reports whether the subscription is still attached.
func (s *Subscription) push(e Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || s.exit != nil {
		return false
	}
	cost := eventCost(e)
	if s.bytes+cost > s.limit {
		s.err = ErrSlowConsumer
		s.queue, s.head, s.bytes = nil, 0, 0
		s.wake()
		return false
	}
	s.queue = append(s.queue, e)
	s.bytes += cost
	s.wake()
	return true
}

func (s *Subscription) finish(exit Exit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.exit = &exit
		s.wake()
	}
}

func (s *Subscription) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}
