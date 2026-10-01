package collab_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
)

const wait = 5 * time.Second

type fakeTimers struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	active := !t.stopped && !t.fired
	t.stopped = true
	return active
}

func (c *fakeTimers) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeTimers) AfterFunc(d time.Duration, f func()) collab.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock and runs due timers.
func (c *fakeTimers) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

func (c *fakeTimers) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

func newHub(t *testing.T, configure func(*collab.Config)) (*collab.Hub, *fakeTimers) {
	t.Helper()
	timers := &fakeTimers{now: time.Unix(1_700_000_000, 0)}
	cfg := collab.Config{Now: timers.Now, AfterFunc: timers.AfterFunc}
	if configure != nil {
		configure(&cfg)
	}
	hub := collab.NewHub(cfg)
	t.Cleanup(hub.Close)
	return hub, timers
}

func join(t *testing.T, hub *collab.Hub, req collab.JoinRequest) *collab.Participant {
	t.Helper()
	if req.TerminalID == "" {
		req.TerminalID = "term"
	}
	p, err := hub.Join(req)
	if err != nil {
		t.Fatalf("Join(%+v): %v", req, err)
	}
	return p
}

func next(t *testing.T, p *collab.Participant) collab.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	event, err := p.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return event
}

func nextType(t *testing.T, p *collab.Participant, want string) collab.Event {
	t.Helper()
	event := next(t, p)
	if event.Type != want {
		t.Fatalf("event = %+v, want type %q", event, want)
	}
	return event
}

func assertNoEvent(t *testing.T, p *collab.Participant) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if event, err := p.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected event %+v, err %v", event, err)
	}
}

// drain discards queued events.
func drain(p *collab.Participant) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := p.Next(ctx)
		cancel()
		if err != nil {
			return
		}
	}
}

func info(p *collab.Participant) collab.ParticipantInfo {
	return collab.ParticipantInfo{ID: p.ID(), Role: p.Role()}
}

func TestPresenceSnapshotJoinAndLeave(t *testing.T) {
	hub, _ := newHub(t, nil)
	owner := join(t, hub, collab.JoinRequest{Role: collab.RoleOwner})
	snapshot := nextType(t, owner, collab.EventPresenceSnapshot)
	if snapshot.Self != owner.ID() || !reflect.DeepEqual(snapshot.Participants, []collab.ParticipantInfo{info(owner)}) {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	viewer := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "grant-secretish", SessionID: 42})
	if viewer.ID() == "" || viewer.ID() == owner.ID() || strings.Contains(viewer.ID(), "grant") || viewer.ID() == "42" {
		t.Fatalf("participant ID %q must be random and unrelated to the grant or session", viewer.ID())
	}
	joined := nextType(t, owner, collab.EventParticipantJoined)
	if *joined.Participant != info(viewer) || joined.Version <= snapshot.Version {
		t.Fatalf("joined = %+v (snapshot version %d)", joined, snapshot.Version)
	}
	viewerSnapshot := nextType(t, viewer, collab.EventPresenceSnapshot)
	if viewerSnapshot.Self != viewer.ID() || viewerSnapshot.Version != joined.Version ||
		!reflect.DeepEqual(viewerSnapshot.Participants, []collab.ParticipantInfo{info(owner), info(viewer)}) {
		t.Fatalf("viewer snapshot = %+v", viewerSnapshot)
	}

	viewer.Leave()
	viewer.Leave()
	left := nextType(t, owner, collab.EventParticipantLeft)
	if *left.Participant != info(viewer) || left.Reason != "disconnected" || left.Version <= joined.Version {
		t.Fatalf("left = %+v", left)
	}
	assertNoEvent(t, owner)
	if stats := hub.Stats(); stats.Rooms != 1 || stats.Participants != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	owner.Leave()
	if stats := hub.Stats(); stats.Rooms != 0 || stats.Participants != 0 {
		t.Fatalf("stats after last leave = %+v, want empty", stats)
	}
}

func TestRolesAndActions(t *testing.T) {
	hub, _ := newHub(t, nil)
	for role, want := range map[collab.Role]map[collab.Action]bool{
		collab.RoleOwner:  {collab.ActionInput: true, collab.ActionResize: true, collab.ActionPing: true},
		collab.RoleEditor: {collab.ActionInput: true, collab.ActionResize: true, collab.ActionPing: true},
		collab.RoleViewer: {collab.ActionInput: false, collab.ActionResize: false, collab.ActionPing: true},
	} {
		p := join(t, hub, collab.JoinRequest{Role: role})
		for action, allowed := range want {
			if got := p.Allowed(action); got != allowed {
				t.Errorf("%s Allowed(%s) = %v, want %v", role, action, got, allowed)
			}
		}
		if got := p.Allowed("exec"); got {
			t.Errorf("%s allowed an unknown action", role)
		}
	}
	if _, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: "admin"}); err == nil {
		t.Fatal("unknown role joined")
	}
}

func TestRevokedGrantLosesPermissionImmediatelyAndIsAnnounced(t *testing.T) {
	hub, _ := newHub(t, nil)
	owner := join(t, hub, collab.JoinRequest{Role: collab.RoleOwner})
	editor := join(t, hub, collab.JoinRequest{Role: collab.RoleEditor, GrantID: "g-editor", SessionID: 1})
	viewer := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "g-viewer", SessionID: 2})
	elsewhere := join(t, hub, collab.JoinRequest{TerminalID: "other", Role: collab.RoleEditor, GrantID: "g-editor", SessionID: 3})
	for _, p := range []*collab.Participant{owner, editor, viewer, elsewhere} {
		drain(p)
	}

	hub.GrantsRevoked("term", []string{"g-editor"}, "replaced")
	if editor.Allowed(collab.ActionInput) || editor.Allowed(collab.ActionResize) || editor.Allowed(collab.ActionPing) {
		t.Fatal("replaced editor still allowed to act")
	}
	if !elsewhere.Allowed(collab.ActionInput) {
		t.Fatal("same grant ID on another terminal lost permission")
	}

	changed := nextType(t, editor, collab.EventPermissionChanged)
	if *changed.Participant != info(editor) || changed.Reason != "replaced" ||
		changed.Permissions == nil || changed.Permissions.Input || changed.Permissions.Resize {
		t.Fatalf("editor permission_changed = %+v", changed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, err := editor.Next(ctx)
	var revoked *collab.RevokedError
	if !errors.As(err, &revoked) || revoked.Reason != "replaced" || !errors.Is(err, collab.ErrRevoked) {
		t.Fatalf("editor Next after revocation err = %v", err)
	}

	for _, observer := range []*collab.Participant{owner, viewer} {
		changed := nextType(t, observer, collab.EventPermissionChanged)
		left := nextType(t, observer, collab.EventParticipantLeft)
		if *changed.Participant != info(editor) || *left.Participant != info(editor) || left.Reason != "replaced" ||
			left.Version <= changed.Version {
			t.Fatalf("observer events = %+v then %+v", changed, left)
		}
	}
	assertNoEvent(t, elsewhere)
	if !viewer.Allowed(collab.ActionPing) || !owner.Allowed(collab.ActionInput) {
		t.Fatal("unrelated participants lost permission")
	}
	if stats := hub.Stats(); stats.Participants != 3 {
		t.Fatalf("stats = %+v, want the revoked participant removed", stats)
	}
}

func TestSessionRevocationAffectsOnlyThatSession(t *testing.T) {
	hub, _ := newHub(t, nil)
	first := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "g", SessionID: 10})
	second := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "g", SessionID: 11})
	nextType(t, first, collab.EventPresenceSnapshot)
	nextType(t, first, collab.EventParticipantJoined)
	nextType(t, second, collab.EventPresenceSnapshot)

	hub.SessionRevoked(10, "logout")
	if first.Allowed(collab.ActionPing) || !second.Allowed(collab.ActionPing) {
		t.Fatal("session revocation hit the wrong participant")
	}
	if e := nextType(t, first, collab.EventPermissionChanged); e.Reason != "logout" {
		t.Fatalf("permission_changed = %+v", e)
	}
	nextType(t, second, collab.EventPermissionChanged)
	if left := nextType(t, second, collab.EventParticipantLeft); left.Participant.ID != first.ID() || left.Reason != "logout" {
		t.Fatalf("left = %+v", left)
	}
}

func TestExpiryIsEnforcedOnEveryActionAndByTimer(t *testing.T) {
	hub, timers := newHub(t, nil)
	owner := join(t, hub, collab.JoinRequest{Role: collab.RoleOwner})
	nextType(t, owner, collab.EventPresenceSnapshot)
	editor := join(t, hub, collab.JoinRequest{Role: collab.RoleEditor, GrantID: "g", SessionID: 1,
		ExpiresAt: timers.Now().Add(time.Minute)})
	nextType(t, editor, collab.EventPresenceSnapshot)
	nextType(t, owner, collab.EventParticipantJoined)
	if !editor.Allowed(collab.ActionInput) {
		t.Fatal("editor not allowed before expiry")
	}

	// A late timer must not extend access: the check itself uses the clock.
	timers.mu.Lock()
	timers.now = timers.now.Add(time.Minute)
	timers.mu.Unlock()
	if editor.Allowed(collab.ActionInput) || editor.Allowed(collab.ActionPing) {
		t.Fatal("expired editor still allowed")
	}
	timers.Advance(0)
	if e := nextType(t, editor, collab.EventPermissionChanged); e.Reason != "expired" {
		t.Fatalf("permission_changed = %+v", e)
	}
	nextType(t, owner, collab.EventPermissionChanged)
	if left := nextType(t, owner, collab.EventParticipantLeft); left.Reason != "expired" {
		t.Fatalf("left = %+v", left)
	}

	// Joining with an expiry already in the past is revoked at once.
	late := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "g2", SessionID: 2, ExpiresAt: timers.Now()})
	timers.Advance(0)
	if late.Allowed(collab.ActionPing) {
		t.Fatal("participant joined past its expiry is allowed")
	}
	if stats := hub.Stats(); stats.Participants != 1 {
		t.Fatalf("stats = %+v, want only the owner", stats)
	}
}

func TestLeaveStopsExpiryTimer(t *testing.T) {
	hub, timers := newHub(t, nil)
	p := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "g", SessionID: 1, ExpiresAt: timers.Now().Add(time.Hour)})
	if timers.pending() != 1 {
		t.Fatalf("pending timers = %d", timers.pending())
	}
	p.Leave()
	if timers.pending() != 0 {
		t.Fatalf("pending timers after leave = %d", timers.pending())
	}
}

func TestSlowParticipantIsDroppedWithoutBlockingOthers(t *testing.T) {
	hub, _ := newHub(t, func(c *collab.Config) { c.QueueLimit = 4 })
	slow := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "g", SessionID: 1})
	owner := join(t, hub, collab.JoinRequest{Role: collab.RoleOwner})
	nextType(t, owner, collab.EventPresenceSnapshot)

	// The owner keeps up by consuming each event before the next change;
	// the slow participant never reads.
	var seen []collab.Event
	await := func(typ, id string) {
		t.Helper()
		for {
			e := next(t, owner)
			seen = append(seen, e)
			if e.Type == typ && e.Participant.ID == id {
				return
			}
		}
	}
	start := time.Now()
	for i := 0; i < 10; i++ {
		p, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer, GrantID: "churn", SessionID: int64(100 + i)})
		if err != nil {
			t.Fatalf("churn join: %v", err)
		}
		await(collab.EventParticipantJoined, p.ID())
		p.Leave()
		await(collab.EventParticipantLeft, p.ID())
	}
	if time.Since(start) > wait {
		t.Fatal("presence broadcast blocked on a slow participant")
	}

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var err error
	for err == nil {
		_, err = slow.Next(ctx)
	}
	if !errors.Is(err, collab.ErrSlowConsumer) {
		t.Fatalf("slow participant err = %v, want ErrSlowConsumer", err)
	}
	if slow.Allowed(collab.ActionPing) {
		t.Fatal("dropped participant still allowed")
	}
	sawSlowLeft := false
	for _, e := range seen {
		if e.Type == collab.EventParticipantLeft && e.Participant.ID == slow.ID() {
			if e.Reason != "slow_consumer" {
				t.Fatalf("left = %+v", e)
			}
			sawSlowLeft = true
		}
	}
	if !sawSlowLeft {
		t.Fatal("owner never saw the slow participant leave")
	}
	if stats := hub.Stats(); stats.Participants != 1 {
		t.Fatalf("stats = %+v, want the owner only", stats)
	}
}

func TestPresenceOrderingIsConsistentUnderConcurrency(t *testing.T) {
	hub, _ := newHub(t, func(c *collab.Config) { c.QueueLimit = 4096 })
	const observers, churners, rounds = 3, 8, 20
	obs := make([]*collab.Participant, observers)
	for i := range obs {
		obs[i] = join(t, hub, collab.JoinRequest{Role: collab.RoleOwner})
	}
	var wg sync.WaitGroup
	for i := 0; i < churners; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				p, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer, GrantID: "g" + strconv.Itoa(i), SessionID: int64(i*1000 + r)})
				if err != nil {
					t.Errorf("join: %v", err)
					return
				}
				p.Leave()
			}
		}(i)
	}
	wg.Wait()
	marker := join(t, hub, collab.JoinRequest{Role: collab.RoleViewer})

	byVersion := map[uint64]string{}
	for i, o := range obs {
		var last uint64
		present := map[string]bool{}
		for {
			e := next(t, o)
			if e.Version <= last && last != 0 {
				t.Fatalf("observer %d: version %d after %d", i, e.Version, last)
			}
			last = e.Version
			switch e.Type {
			case collab.EventPresenceSnapshot:
				for _, p := range e.Participants {
					present[p.ID] = true
				}
				continue
			case collab.EventParticipantJoined:
				present[e.Participant.ID] = true
			case collab.EventParticipantLeft:
				if !present[e.Participant.ID] {
					t.Fatalf("observer %d: %s left before it joined", i, e.Participant.ID)
				}
				delete(present, e.Participant.ID)
			}
			key := fmt.Sprintf("%s/%s", e.Type, e.Participant.ID)
			if prev, ok := byVersion[e.Version]; ok && prev != key {
				t.Fatalf("version %d is %s for one observer and %s for another", e.Version, prev, key)
			}
			byVersion[e.Version] = key
			if e.Type == collab.EventParticipantJoined && e.Participant.ID == marker.ID() {
				break
			}
		}
		if len(present) != observers+1 {
			t.Fatalf("observer %d ends with %d participants present, want %d", i, len(present), observers+1)
		}
	}
	if stats := hub.Stats(); stats.Participants != observers+1 {
		t.Fatalf("stale participants remain: %+v", stats)
	}
}

func TestReconnectGetsFreshIdentityAndRemovesStaleOne(t *testing.T) {
	hub, _ := newHub(t, nil)
	owner := join(t, hub, collab.JoinRequest{Role: collab.RoleOwner})
	nextType(t, owner, collab.EventPresenceSnapshot)
	first := join(t, hub, collab.JoinRequest{Role: collab.RoleEditor, GrantID: "g", SessionID: 1})
	nextType(t, owner, collab.EventParticipantJoined)

	first.Leave()
	second := join(t, hub, collab.JoinRequest{Role: collab.RoleEditor, GrantID: "g", SessionID: 1})
	if left := nextType(t, owner, collab.EventParticipantLeft); left.Participant.ID != first.ID() {
		t.Fatalf("left = %+v", left)
	}
	if joined := nextType(t, owner, collab.EventParticipantJoined); joined.Participant.ID != second.ID() || second.ID() == first.ID() {
		t.Fatalf("joined = %+v", joined)
	}
	snapshot := nextType(t, second, collab.EventPresenceSnapshot)
	for _, p := range snapshot.Participants {
		if p.ID == first.ID() {
			t.Fatal("snapshot contains the stale participant")
		}
	}
	if len(snapshot.Participants) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestEndTerminalAndCloseCleanUp(t *testing.T) {
	hub, timers := newHub(t, func(c *collab.Config) { c.MaxParticipants = 2 })
	a := join(t, hub, collab.JoinRequest{Role: collab.RoleOwner})
	join(t, hub, collab.JoinRequest{Role: collab.RoleViewer, GrantID: "g", SessionID: 1, ExpiresAt: timers.Now().Add(time.Hour)})
	if _, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer}); !errors.Is(err, collab.ErrFull) {
		t.Fatalf("third join err = %v, want ErrFull", err)
	}
	other := join(t, hub, collab.JoinRequest{TerminalID: "other", Role: collab.RoleOwner, ExpiresAt: timers.Now().Add(time.Hour)})
	nextType(t, a, collab.EventPresenceSnapshot)
	nextType(t, a, collab.EventParticipantJoined)

	hub.EndTerminal("term")
	if stats := hub.Stats(); stats.Rooms != 1 || stats.Participants != 1 {
		t.Fatalf("stats after EndTerminal = %+v", stats)
	}
	if timers.pending() != 1 {
		t.Fatalf("pending timers after EndTerminal = %d, want only the other room's", timers.pending())
	}
	a.Leave()
	assertNoEvent(t, a)

	hub.Close()
	hub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		if _, err := other.Next(ctx); err != nil {
			if !errors.Is(err, collab.ErrClosed) {
				t.Fatalf("Next after Close err = %v, want ErrClosed", err)
			}
			break
		}
	}
	if other.Allowed(collab.ActionPing) {
		t.Fatal("participant allowed after Close")
	}
	if _, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleOwner}); !errors.Is(err, collab.ErrClosed) {
		t.Fatalf("Join after Close err = %v", err)
	}
	if stats := hub.Stats(); stats.Rooms != 0 || stats.Participants != 0 || timers.pending() != 0 {
		t.Fatalf("after Close stats = %+v pending timers = %d", stats, timers.pending())
	}
}
