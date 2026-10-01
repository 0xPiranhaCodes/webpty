package access_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// whileRedeeming runs other while a redemption holds its transaction open,
// and returns the redemption's result once both finish.
func whileRedeeming(t *testing.T, h *harness, token, previous string, other func() error) (access.Session, error) {
	t.Helper()
	otherDone := make(chan error, 1)
	session, err := h.service.RedeemReplacing(context.Background(), token, previous, "", func(store.AccessGrant) error {
		go func() { otherDone <- other() }()
		// other is now waiting for the write lock this transaction holds.
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	if otherErr := <-otherDone; otherErr != nil {
		t.Fatalf("concurrent operation: %v", otherErr)
	}
	return session, err
}

func TestRevokeDuringRedemptionEndsTheNewSession(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	invitation := h.grant(access.GrantRequest{Role: store.AccessViewer})
	session, err := whileRedeeming(t, h, invitation.Token, "", func() error {
		_, err := h.service.RevokeGrant(ctx, "term", invitation.Grant.PublicID, "")
		return err
	})
	if err != nil {
		t.Fatalf("redeem committed first, so it succeeds: %v", err)
	}
	if _, err := h.service.Authenticate(ctx, session.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("session redeemed before a revoke survived it: %v", err)
	}
}

func TestRedemptionAfterRevokeFails(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	invitation := h.grant(access.GrantRequest{Role: store.AccessViewer})
	if _, err := h.service.RevokeGrant(ctx, "term", invitation.Grant.PublicID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Redeem(ctx, invitation.Token, "", nil); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("redeem after revoke = %v", err)
	}
}

func TestEditorReplacementDuringRedemptionLeavesOneLiveEditor(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	first := h.grant(access.GrantRequest{Role: store.AccessEditor})
	var replacement access.Invitation
	session, err := whileRedeeming(t, h, first.Token, "", func() error {
		var err error
		replacement, err = h.service.CreateGrant(ctx, access.GrantRequest{TerminalID: "term", Role: store.AccessEditor})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Authenticate(ctx, session.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("replaced editor's session survived: %v", err)
	}
	h.redeem(replacement.Token)
}

func TestRedeemingANewInviteRevokesThePreviousSessionAtomically(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	viewer := h.grant(access.GrantRequest{Role: store.AccessViewer})
	old := h.redeem(viewer.Token)
	other := h.grant(access.GrantRequest{Role: store.AccessViewer})
	// The previous session is revoked concurrently by its grant; either way
	// exactly the new session is live afterwards.
	fresh, err := whileRedeeming(t, h, other.Token, old.Token, func() error {
		_, err := h.service.RevokeGrant(ctx, "term", viewer.Grant.PublicID, "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Authenticate(ctx, old.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("previous session = %v", err)
	}
	if _, err := h.service.Authenticate(ctx, fresh.Token); err != nil {
		t.Fatalf("new session = %v", err)
	}
	superseded := 0
	for _, event := range h.listener.all() {
		if event.reason == access.ReasonSuperseded {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("listener events = %+v, want one supersession", h.listener.all())
	}
}
