package collab_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
)

func TestReservedParticipantIsInvisibleUntilActivated(t *testing.T) {
	observer := &recordingObserver{}
	hub, _ := newHub(t, func(c *collab.Config) { c.Observer = observer })
	owner, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	nextType(t, owner, collab.EventPresenceSnapshot)

	pending, err := hub.Reserve(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer, GrantID: "g", SessionID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n := hub.ParticipantCount("term"); n != 1 {
		t.Fatalf("participants with a reservation = %d, want 1", n)
	}
	if got := observer.snapshot(); len(got) != 1 {
		t.Fatalf("reservation was observed: %v", got)
	}
	if err := pending.Activate(); err != nil {
		t.Fatal(err)
	}
	joined := nextType(t, owner, collab.EventParticipantJoined)
	if joined.Participant.ID != pending.ID() {
		t.Fatalf("joined = %+v", joined)
	}
	snapshot := nextType(t, pending, collab.EventPresenceSnapshot)
	if len(snapshot.Participants) != 2 || snapshot.Self != pending.ID() {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestAbandonedReservationLeavesNoTrace(t *testing.T) {
	observer := &recordingObserver{}
	hub, _ := newHub(t, func(c *collab.Config) { c.Observer = observer })
	owner, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	drain(owner)
	pending, err := hub.Reserve(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	pending.Leave()
	// The next real event is the first one the owner sees.
	hub.SessionRevoked(99, "none")
	other, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	if e := next(t, owner); e.Type != collab.EventParticipantJoined || e.Participant.ID != other.ID() {
		t.Fatalf("owner saw %+v before the real join", e)
	}
	want := []string{"term:participant_joined:owner::false", "term:participant_joined:viewer::false"}
	if got := observer.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observed %v, want %v", got, want)
	}
}

func TestReservationsCountTowardTheLimit(t *testing.T) {
	hub, _ := newHub(t, func(c *collab.Config) { c.MaxParticipants = 1 })
	if _, err := hub.Reserve(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Reserve(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer}); !errors.Is(err, collab.ErrFull) {
		t.Fatalf("second reservation = %v, want ErrFull", err)
	}
}

func TestRevocationBeforeActivationIsSilentAndFinal(t *testing.T) {
	observer := &recordingObserver{}
	hub, _ := newHub(t, func(c *collab.Config) { c.Observer = observer })
	pending, err := hub.Reserve(collab.JoinRequest{TerminalID: "term", Role: collab.RoleEditor, GrantID: "g1", SessionID: 3})
	if err != nil {
		t.Fatal(err)
	}
	hub.GrantsRevoked("term", []string{"g1"}, "replaced")
	var revoked *collab.RevokedError
	if err := pending.Activate(); !errors.As(err, &revoked) || revoked.Reason != "replaced" {
		t.Fatalf("Activate after revocation = %v, want RevokedError(replaced)", err)
	}
	if pending.Allowed(collab.ActionInput) {
		t.Fatal("revoked reservation may still type")
	}
	if got := observer.snapshot(); len(got) != 0 {
		t.Fatalf("revoked reservation was observed: %v", got)
	}
	if stats := hub.Stats(); stats.Participants != 0 || stats.Rooms != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestActivatingAfterTheTerminalEndedIsANoOp(t *testing.T) {
	hub, _ := newHub(t, nil)
	pending, err := hub.Reserve(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	hub.EndTerminal("term")
	if err := pending.Activate(); err != nil {
		t.Fatalf("Activate after EndTerminal = %v", err)
	}
	pending.Leave()
	if stats := hub.Stats(); stats.Participants != 0 || stats.Rooms != 0 {
		t.Fatalf("ended terminal left presence behind: %+v", stats)
	}
}
