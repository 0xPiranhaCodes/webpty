package access_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// abortWhen installs a trigger that aborts the statement it guards, and
// returns a function that removes it.
func (h *harness) abortWhen(name, guard string) func() {
	h.t.Helper()
	db := h.store.DB()
	if _, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER %s %s BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, name, guard)); err != nil {
		h.t.Fatal(err)
	}
	removed := false
	remove := func() {
		if !removed {
			removed = true
			if _, err := db.Exec(`DROP TRIGGER ` + name); err != nil {
				h.t.Fatal(err)
			}
		}
	}
	h.t.Cleanup(remove)
	return remove
}

func (h *harness) failAudit(eventType string) func() {
	return h.abortWhen("fail_audit", fmt.Sprintf(`BEFORE INSERT ON audit_events WHEN NEW.event_type = '%s'`, eventType))
}

func (h *harness) count(query string) int {
	h.t.Helper()
	var n int
	if err := h.store.DB().QueryRow(query).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) audited(eventType string) int {
	return h.count(fmt.Sprintf(`SELECT count(*) FROM audit_events WHERE event_type = '%s'`, eventType))
}

func (h *harness) grants() []store.AccessGrant {
	h.t.Helper()
	grants, err := h.service.Grants(context.Background(), "term")
	if err != nil {
		h.t.Fatal(err)
	}
	return grants
}

func (h *harness) mustAuthenticate(token string) {
	h.t.Helper()
	if _, err := h.service.Authenticate(context.Background(), token); err != nil {
		h.t.Fatalf("session no longer authenticates: %v", err)
	}
}

func (h *harness) noNotifications() {
	h.t.Helper()
	if events := h.listener.all(); len(events) != 0 {
		h.t.Fatalf("listener notified of rolled-back change: %+v", events)
	}
}

func TestCreateGrantRollsBackWhenItsAuditFails(t *testing.T) {
	h := newHarness(t, nil)
	h.failAudit("access.grant.created")

	if _, err := h.service.CreateGrant(context.Background(), access.GrantRequest{TerminalID: "term", Role: store.AccessViewer}); err == nil {
		t.Fatal("CreateGrant succeeded without its audit event")
	}
	if grants := h.grants(); len(grants) != 0 {
		t.Fatalf("grants = %+v, want none", grants)
	}
	h.noNotifications()
}

func TestEditorReplacementRollsBackWhenAnyAuditFails(t *testing.T) {
	for _, event := range []string{"access.grant.replaced", "access.grant.created"} {
		t.Run(event, func(t *testing.T) {
			h := newHarness(t, nil)
			old := h.grant(access.GrantRequest{Role: store.AccessEditor})
			oldSession := h.redeem(old.Token)
			remove := h.failAudit(event)

			if _, err := h.service.CreateGrant(context.Background(), access.GrantRequest{TerminalID: "term", Role: store.AccessEditor}); err == nil {
				t.Fatal("CreateGrant succeeded without its audit events")
			}
			grants := h.grants()
			if len(grants) != 1 || access.Status(grants[0], h.clock.Now()) != access.StatusActive {
				t.Fatalf("grants = %+v, want only the previous editor, still active", grants)
			}
			h.mustAuthenticate(oldSession.Token)
			h.noNotifications()
			if h.audited("access.grant.replaced") != 0 {
				t.Fatal("replacement audited although it was rolled back")
			}

			remove()
			replacement := h.grant(access.GrantRequest{Role: store.AccessEditor})
			if len(replacement.Replaced) != 1 || len(h.listener.all()) != 1 {
				t.Fatalf("replacement after recovery = %+v, notifications %+v", replacement.Replaced, h.listener.all())
			}
		})
	}
}

func TestRedeemRollsBackWhenAuditOrStoreFails(t *testing.T) {
	for name, inject := range map[string]func(*harness) func(){
		"audit":   func(h *harness) func() { return h.failAudit("access.grant.redeemed") },
		"session": func(h *harness) func() { return h.abortWhen("fail_session", `BEFORE INSERT ON access_sessions`) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			invitation := h.grant(access.GrantRequest{Role: store.AccessViewer, SingleUse: true})
			remove := inject(h)

			if _, err := h.service.Redeem(context.Background(), invitation.Token, "198.51.100.7", nil); err == nil {
				t.Fatal("Redeem succeeded despite the injected failure")
			}
			h.assertUnredeemed(invitation.Token)

			remove()
			h.redeem(invitation.Token)
		})
	}
}

func (h *harness) assertUnredeemed(token string) {
	h.t.Helper()
	grants := h.grants()
	if len(grants) != 1 || grants[0].RedemptionCount != 0 || !grants[0].RedeemedAt.IsZero() {
		h.t.Fatalf("grant = %+v, want unredeemed", grants)
	}
	if n := h.count(`SELECT count(*) FROM access_sessions`); n != 0 {
		h.t.Fatalf("access sessions = %d, want 0", n)
	}
	if h.audited("access.grant.redeemed") != 0 {
		h.t.Fatal("redemption audited although it was rolled back")
	}
}

func TestRedeemCheckRunsBeforeCommit(t *testing.T) {
	h := newHarness(t, nil)
	invitation := h.grant(access.GrantRequest{Role: store.AccessViewer, SingleUse: true})
	lookup := errors.New("terminal metadata unavailable")

	var checked store.AccessGrant
	_, err := h.service.Redeem(context.Background(), invitation.Token, "198.51.100.7", func(grant store.AccessGrant) error {
		checked = grant
		return lookup
	})
	if !errors.Is(err, lookup) {
		t.Fatalf("Redeem = %v, want the check's error", err)
	}
	if checked.PublicID != invitation.Grant.PublicID || checked.TerminalID != "term" {
		t.Fatalf("check saw %+v", checked)
	}
	h.assertUnredeemed(invitation.Token)
	h.redeem(invitation.Token)
}

func TestRevokeGrantRollsBackWhenItsAuditFails(t *testing.T) {
	h := newHarness(t, nil)
	invitation := h.grant(access.GrantRequest{Role: store.AccessViewer})
	session := h.redeem(invitation.Token)
	remove := h.failAudit("access.grant.revoked")

	if _, err := h.service.RevokeGrant(context.Background(), "term", invitation.Grant.PublicID, "198.51.100.7"); err == nil {
		t.Fatal("RevokeGrant succeeded without its audit event")
	}
	if grants := h.grants(); access.Status(grants[0], h.clock.Now()) != access.StatusActive {
		t.Fatalf("grant = %+v, want active", grants[0])
	}
	h.mustAuthenticate(session.Token)
	h.noNotifications()

	remove()
	if _, err := h.service.RevokeGrant(context.Background(), "term", invitation.Grant.PublicID, "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	if len(h.listener.all()) != 1 || h.audited("access.grant.revoked") != 1 {
		t.Fatalf("after recovery: notifications %+v, audits %d", h.listener.all(), h.audited("access.grant.revoked"))
	}
}

func TestLogoutRollsBackWhenItsAuditFails(t *testing.T) {
	h := newHarness(t, nil)
	session := h.redeem(h.grant(access.GrantRequest{Role: store.AccessViewer}).Token)
	remove := h.failAudit("access.logout")

	if err := h.service.Logout(context.Background(), session.Token, "198.51.100.7"); err == nil {
		t.Fatal("Logout succeeded without its audit event")
	}
	h.mustAuthenticate(session.Token)
	h.noNotifications()

	remove()
	if err := h.service.Logout(context.Background(), session.Token, "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	if len(h.listener.all()) != 1 || h.audited("access.logout") != 1 {
		t.Fatalf("after recovery: notifications %+v, audits %d", h.listener.all(), h.audited("access.logout"))
	}
}

// operations exercises every state-changing Service call against a fresh
// fixture: a live editor grant with a session, which each call would change.
type operation struct {
	name  string
	run   func(ctx context.Context, h *harness, f fixture) error
	event string
}

type fixture struct {
	editor  access.Invitation
	session access.Session
	viewer  access.Invitation
}

var operations = []operation{
	{"create replacing editor", func(ctx context.Context, h *harness, _ fixture) error {
		_, err := h.service.CreateGrant(ctx, access.GrantRequest{TerminalID: "term", Role: store.AccessEditor})
		return err
	}, "access.grant.created"},
	{"redeem", func(ctx context.Context, h *harness, f fixture) error {
		_, err := h.service.Redeem(ctx, f.viewer.Token, "198.51.100.7", nil)
		return err
	}, "access.grant.redeemed"},
	{"revoke", func(ctx context.Context, h *harness, f fixture) error {
		_, err := h.service.RevokeGrant(ctx, "term", f.editor.Grant.PublicID, "198.51.100.7")
		return err
	}, "access.grant.revoked"},
	{"logout", func(ctx context.Context, h *harness, f fixture) error {
		return h.service.Logout(ctx, f.session.Token, "198.51.100.7")
	}, "access.logout"},
}

func newFixture(h *harness) fixture {
	editor := h.grant(access.GrantRequest{Role: store.AccessEditor})
	return fixture{
		editor:  editor,
		session: h.redeem(editor.Token),
		viewer:  h.grant(access.GrantRequest{Role: store.AccessViewer, SingleUse: true}),
	}
}

// assertUnchanged checks that f is exactly as newFixture left it.
func (h *harness) assertUnchanged(f fixture, event string) {
	h.t.Helper()
	grants := h.grants()
	if len(grants) != 2 {
		h.t.Fatalf("grants = %+v, want the fixture's two", grants)
	}
	for _, grant := range grants {
		if access.Status(grant, h.clock.Now()) != access.StatusActive {
			h.t.Fatalf("grant %+v is no longer active", grant)
		}
	}
	if grants[1].RedemptionCount != 0 {
		h.t.Fatalf("single-use viewer grant consumed: %+v", grants[1])
	}
	if n := h.count(`SELECT count(*) FROM access_sessions`); n != 1 {
		h.t.Fatalf("access sessions = %d, want 1", n)
	}
	h.mustAuthenticate(f.session.Token)
	h.noNotifications()
	want := 0
	if event == "access.grant.created" {
		want = 2
	}
	if event == "access.grant.redeemed" {
		want = 1
	}
	if got := h.audited(event); got != want {
		h.t.Fatalf("%s audits = %d, want %d", event, got, want)
	}
	if h.audited("access.grant.replaced") != 0 {
		h.t.Fatal("rolled-back replacement was audited")
	}
}

func TestCancellationBeforeCallChangesNothing(t *testing.T) {
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			h := newHarness(t, nil)
			f := newFixture(h)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := op.run(ctx, h, f); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			h.assertUnchanged(f, op.event)
		})
	}
}

func TestCancellationInsideTransactionRollsBack(t *testing.T) {
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			h := newHarness(t, nil)
			f := newFixture(h)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hooked := 0
			access.SetBeforeCommit(h.service, func(context.Context) error {
				hooked++
				cancel()
				return nil
			})

			if err := op.run(ctx, h, f); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if hooked != 1 {
				t.Fatalf("before-commit hook ran %d times, want 1", hooked)
			}
			access.SetBeforeCommit(h.service, nil)
			h.assertUnchanged(f, op.event)
		})
	}
}

func TestFailureJustBeforeCommitRollsBack(t *testing.T) {
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			h := newHarness(t, nil)
			f := newFixture(h)
			injected := errors.New("injected store failure")
			access.SetBeforeCommit(h.service, func(context.Context) error { return injected })

			if err := op.run(context.Background(), h, f); !errors.Is(err, injected) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
			access.SetBeforeCommit(h.service, nil)
			h.assertUnchanged(f, op.event)
			// The rolled-back call can be retried with the same credentials.
			if err := op.run(context.Background(), h, f); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

// cancelOnNotify cancels the caller's context as soon as the listener runs,
// that is, right after the transaction commits.
type cancelOnNotify struct {
	access.Listener
	cancel context.CancelFunc
}

func (c *cancelOnNotify) GrantsRevoked(terminalID string, grantIDs []string, reason string) {
	c.cancel()
	c.Listener.GrantsRevoked(terminalID, grantIDs, reason)
}

func (c *cancelOnNotify) SessionRevoked(sessionID int64, reason string) {
	c.cancel()
	c.Listener.SessionRevoked(sessionID, reason)
}

func TestCancellationAfterCommitKeepsChangeAndAudit(t *testing.T) {
	for _, op := range operations {
		if op.name == "redeem" {
			continue // Redeem notifies no listener.
		}
		t.Run(op.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := newHarness(t, func(cfg *access.Config) {
				cfg.Listener = &cancelOnNotify{Listener: cfg.Listener, cancel: cancel}
			})
			f := newFixture(h)
			before := h.audited(op.event)

			if err := op.run(ctx, h, f); err != nil {
				t.Fatalf("committed %s reported %v after cancellation", op.name, err)
			}
			if ctx.Err() == nil {
				t.Fatal("listener did not run")
			}
			if got := h.audited(op.event); got != before+1 {
				t.Fatalf("%s audits = %d, want %d", op.event, got, before+1)
			}
			if _, err := h.service.Authenticate(context.Background(), f.session.Token); err == nil {
				t.Fatal("editor session survived the committed change")
			}
		})
	}
}
