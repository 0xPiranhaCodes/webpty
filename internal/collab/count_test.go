package collab_test

import (
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/collab"
)

func TestParticipantCountTracksJoinsAndLeaves(t *testing.T) {
	hub := collab.NewHub(collab.Config{})
	t.Cleanup(hub.Close)
	if got := hub.ParticipantCount("t1"); got != 0 {
		t.Fatalf("empty count = %d", got)
	}
	owner, err := hub.Join(collab.JoinRequest{TerminalID: "t1", Role: collab.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := hub.Join(collab.JoinRequest{TerminalID: "t1", Role: collab.RoleViewer, GrantID: "g1", SessionID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Join(collab.JoinRequest{TerminalID: "t2", Role: collab.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if got := hub.ParticipantCount("t1"); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	viewer.Leave()
	owner.Leave()
	if got := hub.ParticipantCount("t1"); got != 0 {
		t.Fatalf("count after leave = %d", got)
	}
	if got := hub.ParticipantCount("t2"); got != 1 {
		t.Fatalf("other terminal = %d", got)
	}
}
