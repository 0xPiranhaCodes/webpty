// Package collab tracks who is connected to each terminal, what each
// participant may do, and announces presence and permission changes.
package collab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Role is a participant's authority over a terminal.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Action is something a participant asks the terminal to do.
type Action string

const (
	ActionInput  Action = "input"
	ActionResize Action = "resize"
	ActionPing   Action = "ping"
)

// Allows reports whether the role permits action.
func (r Role) Allows(action Action) bool {
	switch action {
	case ActionPing:
		return r == RoleOwner || r == RoleEditor || r == RoleViewer
	case ActionInput, ActionResize:
		return r == RoleOwner || r == RoleEditor
	default:
		return false
	}
}

// Server event types.
const (
	EventPresenceSnapshot  = "presence_snapshot"
	EventParticipantJoined = "participant_joined"
	EventParticipantLeft   = "participant_left"
	EventPermissionChanged = "permission_changed"
)

// Participant-left reasons besides revocation reasons.
const (
	ReasonDisconnected = "disconnected"
	ReasonSlowConsumer = "slow_consumer"
	ReasonExpired      = "expired"
)

var (
	ErrClosed       = errors.New("collab: hub closed")
	ErrFull         = errors.New("collab: participant limit reached")
	ErrSlowConsumer = errors.New("collab: slow consumer")
	ErrRevoked      = errors.New("collab: access revoked")
	ErrInvalidRole  = errors.New("collab: invalid role")
	errLeft         = errors.New("collab: participant left")
)

// RevokedError reports why a participant lost access. It matches ErrRevoked.
type RevokedError struct{ Reason string }

func (e *RevokedError) Error() string        { return "collab: access revoked: " + e.Reason }
func (e *RevokedError) Is(target error) bool { return target == ErrRevoked }

// ParticipantInfo is the public identity of a participant. It never carries
// credentials or network addresses.
type ParticipantInfo struct {
	ID   string `json:"id"`
	Role Role   `json:"role"`
}

// Permissions are the actions a participant may currently take.
type Permissions struct {
	Input  bool `json:"input"`
	Resize bool `json:"resize"`
}

// Event is a presence or permission message. Version increases with every
// change to a terminal's participants, so all participants observe changes
// in the same order.
type Event struct {
	Type         string            `json:"type"`
	Version      uint64            `json:"version"`
	Self         string            `json:"self,omitempty"`
	Participants []ParticipantInfo `json:"participants,omitempty"`
	Participant  *ParticipantInfo  `json:"participant,omitempty"`
	Permissions  *Permissions      `json:"permissions,omitempty"`
	Reason       string            `json:"reason,omitempty"`
}

// Timer is a stoppable pending callback.
type Timer interface {
	Stop() bool
}

// Config tunes a Hub. Zero fields receive defaults.
type Config struct {
	Now       func() time.Time
	AfterFunc func(time.Duration, func()) Timer
	// QueueLimit bounds the events waiting for one participant; a
	// participant that falls further behind is dropped.
	QueueLimit int
	// MaxParticipants bounds the participants of one terminal.
	MaxParticipants int
	// Observer, if set, is told about every join, leave, and permission
	// change.
	Observer Observer
}

// Observer receives presence changes in the order participants see them.
// It is called with the hub's lock held, so it must return without blocking
// and must not call back into the hub.
type Observer interface {
	PresenceChanged(terminalID string, e Event)
}

// Hub holds the live participants of every terminal.
type Hub struct {
	cfg Config

	mu     sync.Mutex
	rooms  map[string]*room
	closed bool
}

type room struct {
	id           string
	version      uint64
	participants []*Participant
}

// NewHub returns an empty Hub.
func NewHub(cfg Config) *Hub {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	if cfg.QueueLimit <= 0 {
		cfg.QueueLimit = 256
	}
	if cfg.MaxParticipants <= 0 {
		cfg.MaxParticipants = 64
	}
	return &Hub{cfg: cfg, rooms: map[string]*room{}}
}

// JoinRequest identifies a connecting participant. GrantID and SessionID
// tie a guest to its capability so revocations can find it; a zero
// ExpiresAt never expires.
type JoinRequest struct {
	TerminalID string
	Role       Role
	GrantID    string
	SessionID  int64
	ExpiresAt  time.Time
}

// Join adds a participant at once: Reserve followed by Activate.
func (h *Hub) Join(req JoinRequest) (*Participant, error) {
	p, err := h.Reserve(req)
	if err != nil {
		return nil, err
	}
	if err := p.Activate(); err != nil {
		p.Leave()
		return nil, err
	}
	return p, nil
}

// Reserve holds a place for a participant without announcing it. The
// reservation counts toward the participant limit and can be revoked or
// expire, but nobody, observers included, learns about it until Activate.
// Leave abandons it without a trace.
func (h *Hub) Reserve(req JoinRequest) (*Participant, error) {
	if req.Role != RoleOwner && req.Role != RoleEditor && req.Role != RoleViewer {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRole, req.Role)
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	r := h.rooms[req.TerminalID]
	if r == nil {
		r = &room{id: req.TerminalID}
		h.rooms[req.TerminalID] = r
	}
	if len(r.participants) >= h.cfg.MaxParticipants {
		if len(r.participants) == 0 {
			delete(h.rooms, req.TerminalID)
		}
		return nil, ErrFull
	}
	p := &Participant{
		hub: h, room: r, id: id, role: req.Role, grantID: req.GrantID, sessionID: req.SessionID,
		expiresAt: req.ExpiresAt, limit: h.cfg.QueueLimit, notify: make(chan struct{}, 1),
	}
	r.participants = append(r.participants, p)
	if !req.ExpiresAt.IsZero() {
		p.timer = h.cfg.AfterFunc(req.ExpiresAt.Sub(h.cfg.Now()), func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.revokeLocked(p, ReasonExpired)
		})
	}
	return p, nil
}

// Activate announces a reserved participant: it first receives a presence
// snapshot, then everyone else is told it joined. It returns the
// *RevokedError of a reservation revoked or expired meanwhile, and ErrClosed
// once the hub is closed. A reservation whose terminal ended stays detached
// and Activate does nothing; its connection ends with the session.
func (p *Participant) Activate() error {
	h := p.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.active {
		return nil
	}
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	if err != nil && err != errLeft {
		return err
	}
	r := p.room
	if r == nil {
		return nil
	}
	p.active = true
	r.version++
	snapshot := Event{Type: EventPresenceSnapshot, Version: r.version, Self: p.id,
		Participants: make([]ParticipantInfo, 0, len(r.participants))}
	for _, member := range r.participants {
		if member.active {
			snapshot.Participants = append(snapshot.Participants, member.info())
		}
	}
	p.push(snapshot, false)
	joined := p.info()
	h.announceLocked(r, Event{Type: EventParticipantJoined, Version: r.version, Participant: &joined}, p)
	return nil
}

// GrantsRevoked removes the terminal's participants admitted by grantIDs.
func (h *Hub) GrantsRevoked(terminalID string, grantIDs []string, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[terminalID]
	if r == nil {
		return
	}
	revoked := map[string]bool{}
	for _, id := range grantIDs {
		revoked[id] = true
	}
	for _, p := range append([]*Participant(nil), r.participants...) {
		if p.grantID != "" && revoked[p.grantID] {
			h.revokeLocked(p, reason)
		}
	}
}

// SessionRevoked removes every participant admitted by the access session.
func (h *Hub) SessionRevoked(sessionID int64, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rooms {
		for _, p := range append([]*Participant(nil), r.participants...) {
			if p.sessionID != 0 && p.sessionID == sessionID {
				h.revokeLocked(p, reason)
			}
		}
	}
}

// EndTerminal forgets a terminal whose session ended. Its participants are
// detached silently; their connections end with the session.
func (h *Hub) EndTerminal(terminalID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[terminalID]
	if r == nil {
		return
	}
	for _, p := range r.participants {
		p.detachLocked()
	}
	r.participants = nil
	delete(h.rooms, terminalID)
}

// Close ends every participant with ErrClosed and rejects later joins. It
// is idempotent.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for id, r := range h.rooms {
		for _, p := range r.participants {
			p.detachLocked()
			p.fail(ErrClosed, true)
		}
		r.participants = nil
		delete(h.rooms, id)
	}
}

// Stats counts live terminals and participants.
type Stats struct {
	Rooms, Participants int
}

// Stats reports the hub's current size.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	stats := Stats{Rooms: len(h.rooms)}
	for _, r := range h.rooms {
		stats.Participants += len(r.participants)
	}
	return stats
}

// ParticipantCount reports how many announced participants terminalID has
// now; reservations are not counted.
func (h *Hub) ParticipantCount(terminalID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	if r, ok := h.rooms[terminalID]; ok {
		for _, p := range r.participants {
			if p.active {
				n++
			}
		}
	}
	return n
}

// revokeLocked strips p's permissions, tells everyone (p included), then
// removes p. p's final events are delivered before it sees RevokedError.
func (h *Hub) revokeLocked(p *Participant, reason string) {
	if p.room == nil {
		return
	}
	p.denied.Store(true)
	if !p.active {
		h.removeLocked(p, reason, &RevokedError{Reason: reason}, true)
		return
	}
	r := p.room
	r.version++
	who := p.info()
	changed := Event{Type: EventPermissionChanged, Version: r.version, Participant: &who,
		Permissions: &Permissions{}, Reason: reason}
	p.push(changed, true)
	h.announceLocked(r, changed, p)
	h.removeLocked(p, reason, &RevokedError{Reason: reason}, false)
}

// removeLocked takes p out of its room and announces why.
func (h *Hub) removeLocked(p *Participant, reason string, err error, clearQueue bool) {
	r := p.room
	if r == nil {
		return
	}
	p.detachLocked()
	p.fail(err, clearQueue)
	for i, member := range r.participants {
		if member == p {
			r.participants = append(r.participants[:i], r.participants[i+1:]...)
			break
		}
	}
	if !p.active {
		if len(r.participants) == 0 {
			delete(h.rooms, r.id)
		}
		return
	}
	r.version++
	who := p.info()
	left := Event{Type: EventParticipantLeft, Version: r.version, Participant: &who, Reason: reason}
	if len(r.participants) == 0 {
		h.observeLocked(r, left)
		delete(h.rooms, r.id)
		return
	}
	h.announceLocked(r, left, nil)
}

// announceLocked tells the observer about e, then broadcasts it.
func (h *Hub) announceLocked(r *room, e Event, skip *Participant) {
	h.observeLocked(r, e)
	h.broadcastLocked(r, e, skip)
}

func (h *Hub) observeLocked(r *room, e Event) {
	if h.cfg.Observer != nil {
		h.cfg.Observer.PresenceChanged(r.id, e)
	}
}

// broadcastLocked queues e for every participant except skip, dropping any
// participant whose queue is full.
func (h *Hub) broadcastLocked(r *room, e Event, skip *Participant) {
	var slow []*Participant
	for _, p := range r.participants {
		if p != skip && p.active && !p.push(e, false) {
			slow = append(slow, p)
		}
	}
	for _, p := range slow {
		h.removeLocked(p, ReasonSlowConsumer, ErrSlowConsumer, true)
	}
}

// Participant is one connection's presence in a terminal.
type Participant struct {
	hub       *Hub
	id        string
	role      Role
	grantID   string
	sessionID int64
	expiresAt time.Time
	limit     int

	// Guarded by hub.mu. active is set once the participant is announced.
	room   *room
	timer  Timer
	active bool

	denied atomic.Bool

	mu     sync.Mutex
	queue  []Event
	err    error
	notify chan struct{}
}

// ID is the participant's public, non-secret identifier.
func (p *Participant) ID() string { return p.id }

// Role is the participant's role.
func (p *Participant) Role() Role { return p.role }

func (p *Participant) info() ParticipantInfo { return ParticipantInfo{ID: p.id, Role: p.role} }

// Allowed reports whether the participant may take action now. Revocation
// and expiry take effect here immediately, independently of event delivery.
func (p *Participant) Allowed(action Action) bool {
	if p.denied.Load() || !p.role.Allows(action) {
		return false
	}
	return p.expiresAt.IsZero() || p.hub.cfg.Now().Before(p.expiresAt)
}

// Next returns the next queued event. Once the participant is removed it
// returns the remaining events and then the reason: *RevokedError,
// ErrSlowConsumer, or ErrClosed. A detached participant just waits for ctx.
func (p *Participant) Next(ctx context.Context) (Event, error) {
	for {
		p.mu.Lock()
		if len(p.queue) > 0 {
			e := p.queue[0]
			p.queue[0] = Event{}
			p.queue = p.queue[1:]
			p.mu.Unlock()
			return e, nil
		}
		err := p.err
		p.mu.Unlock()
		if err != nil && err != errLeft {
			return Event{}, err
		}
		select {
		case <-p.notify:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// Leave removes the participant and tells the others it disconnected. It is
// idempotent.
func (p *Participant) Leave() {
	p.hub.mu.Lock()
	defer p.hub.mu.Unlock()
	p.hub.removeLocked(p, ReasonDisconnected, errLeft, true)
}

// push queues e and reports false if the queue is full. final events are
// queued regardless of the limit.
func (p *Participant) push(e Event, final bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return true
	}
	if !final && len(p.queue) >= p.limit {
		return false
	}
	p.queue = append(p.queue, e)
	p.wake()
	return true
}

func (p *Participant) fail(err error, clearQueue bool) {
	p.denied.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
	if clearQueue {
		p.queue = nil
	}
	p.wake()
}

// detachLocked removes p from hub bookkeeping without announcing anything.
func (p *Participant) detachLocked() {
	p.denied.Store(true)
	p.room = nil
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

func (p *Participant) wake() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func newID() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("collab: participant id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
