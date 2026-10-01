package collab_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
)

type recordingObserver struct {
	mu     sync.Mutex
	events []string
}

func (o *recordingObserver) PresenceChanged(terminalID string, e collab.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, fmt.Sprintf("%s:%s:%s:%s:%s", terminalID, e.Type, e.Participant.Role, e.Reason,
		fmt.Sprint(e.Permissions != nil)))
}

func (o *recordingObserver) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func TestObserverSeesJoinLeaveAndPermissionChanges(t *testing.T) {
	observer := &recordingObserver{}
	hub, _ := newHub(t, func(c *collab.Config) { c.Observer = observer })

	owner, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleViewer, GrantID: "g1", SessionID: 7}); err != nil {
		t.Fatal(err)
	}
	hub.GrantsRevoked("term", []string{"g1"}, "revoked")
	owner.Leave()

	want := []string{
		"term:participant_joined:owner::false",
		"term:participant_joined:viewer::false",
		"term:permission_changed:viewer:revoked:true",
		"term:participant_left:viewer:revoked:false",
		"term:participant_left:owner:disconnected:false",
	}
	if got := observer.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observed %v\nwant     %v", got, want)
	}
}

func TestObserverMayBeAbsent(t *testing.T) {
	hub, _ := newHub(t, nil)
	p, err := hub.Join(collab.JoinRequest{TerminalID: "term", Role: collab.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	p.Leave()
}
