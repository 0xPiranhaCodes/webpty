package auth_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// concurrently runs fn on another goroutine and waits for it, standing in for
// a second request that interleaves at a hook point.
func concurrently(fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
}

// once returns a hook that runs fn on its first invocation only, without
// blocking re-entrant calls made by fn itself.
func once(fn func()) func() {
	var fired atomic.Bool
	return func() {
		if fired.CompareAndSwap(false, true) {
			fn()
		}
	}
}

func countSessions(t *testing.T, s *store.Store, kind store.SessionKind) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM admin_sessions WHERE kind = ?`, string(kind)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func auditEvents(t *testing.T, s *store.Store) []store.AuditEvent {
	t.Helper()
	events, err := s.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestLoginRejectedWhenPasswordRotatesAfterVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.bootstrap(t)

	verified := make(chan struct{})
	proceed := make(chan struct{})
	auth.SetLoginVerifiedHook(h.service, once(func() {
		close(verified)
		<-proceed
	}))
	result := make(chan error, 1)
	go func() {
		_, err := h.service.Login(ctx, newPassword, "192.0.2.7")
		result <- err
	}()

	<-verified
	rotated, err := h.service.ChangePassword(ctx, admin.Token, newPassword, "rotated passphrase value", "192.0.2.1")
	if err != nil {
		t.Fatalf("concurrent rotation: %v", err)
	}
	close(proceed)

	if err := <-result; !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("Login verified against the old password err = %v, want ErrInvalidCredentials", err)
	}
	if n := countSessions(t, h.store, store.SessionKindAdmin); n != 1 {
		t.Errorf("admin sessions = %d, want only the rotation's fresh session", n)
	}
	if _, err := h.service.Authenticate(ctx, rotated.Token); err != nil {
		t.Errorf("rotation session rejected: %v", err)
	}
}

func TestBootstrapLoginRejectedWhenBootstrapCompletesAfterVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	verified := make(chan struct{})
	proceed := make(chan struct{})
	auth.SetLoginVerifiedHook(h.service, once(func() {
		close(verified)
		<-proceed
	}))
	result := make(chan error, 1)
	go func() {
		_, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.7")
		result <- err
	}()

	<-verified
	if _, err := h.service.ChangePassword(ctx, first.Token, auth.DefaultPassword, newPassword, "192.0.2.1"); err != nil {
		t.Fatalf("concurrent bootstrap rotation: %v", err)
	}
	close(proceed)

	if err := <-result; !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("stale bootstrap login err = %v, want ErrInvalidCredentials", err)
	}
	if n := countSessions(t, h.store, store.SessionKindBootstrap); n != 0 {
		t.Errorf("bootstrap sessions = %d after initialization, want 0", n)
	}
}

func TestPasswordChangeThrottledPerSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.bootstrap(t)

	for i := range 3 {
		if _, err := h.service.ChangePassword(ctx, admin.Token, "wrong current", "another good passphrase", "192.0.2.50"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d err = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	if _, err := h.service.ChangePassword(ctx, admin.Token, newPassword, "another good passphrase", "198.51.100.5"); !errors.Is(err, auth.ErrThrottled) {
		t.Fatalf("same session from new IP err = %v, want ErrThrottled", err)
	}
	if _, err := h.service.Login(ctx, newPassword, "192.0.2.1"); err != nil {
		t.Fatalf("password changed despite throttle: %v", err)
	}

	h.clock.Advance(time.Minute)
	if _, err := h.service.ChangePassword(ctx, admin.Token, newPassword, "another good passphrase", "198.51.100.5"); err != nil {
		t.Fatalf("attempt after window: %v", err)
	}
}

func TestPasswordChangeThrottledPerIP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := h.bootstrap(t)
	second, err := h.service.Login(ctx, newPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		_, _ = h.service.ChangePassword(ctx, first.Token, "wrong current", "another good passphrase", "203.0.113.9")
	}
	if _, err := h.service.ChangePassword(ctx, second.Token, newPassword, "another good passphrase", "203.0.113.9"); !errors.Is(err, auth.ErrThrottled) {
		t.Fatalf("other session from same IP err = %v, want ErrThrottled", err)
	}
}

func TestPasswordChangeFailuresAreAuditedWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.bootstrap(t)

	for range 4 {
		_, _ = h.service.ChangePassword(ctx, admin.Token, "wrong-current-secret", "candidate-new-secret", "192.0.2.50")
	}

	var failures, throttled int
	for _, event := range auditEvents(t, h.store) {
		switch event.Type {
		case "admin.password.failure":
			failures++
			if event.Details["reason"] != "invalid_current_password" {
				t.Errorf("failure details = %v", event.Details)
			}
		case "admin.password.throttled":
			throttled++
		}
	}
	if failures != 3 || throttled != 1 {
		t.Errorf("failures = %d, throttled = %d; want 3 and 1", failures, throttled)
	}
	assertDatabaseFreeOf(t, h.store, []string{"wrong-current-secret", "candidate-new-secret", admin.Token})
}

func TestPasswordChangeDoesNotHoldWriteLockDuringVerification(t *testing.T) {
	for _, bootstrap := range []bool{true, false} {
		name := "admin"
		if bootstrap {
			name = "bootstrap"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			token, current := "", newPassword
			if bootstrap {
				session, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
				if err != nil {
					t.Fatal(err)
				}
				token, current = session.Token, auth.DefaultPassword
			} else {
				token = h.bootstrap(t).Token
			}

			var writeErr error
			var writeTook time.Duration
			auth.SetPasswordChangeVerifyHook(h.service, once(func() {
				concurrently(func() {
					start := time.Now()
					writeErr = h.store.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: h.clock.Now(), Type: "test.concurrent.write"})
					writeTook = time.Since(start)
				})
			}))

			if _, err := h.service.ChangePassword(ctx, token, current, "another good passphrase", "192.0.2.1"); err != nil {
				t.Fatalf("ChangePassword: %v", err)
			}
			if writeErr != nil || writeTook > time.Second {
				t.Fatalf("concurrent write during verification took %v err %v; write lock held while verifying", writeTook, writeErr)
			}
		})
	}
}

func TestPasswordChangeRejectedWhenSessionRevokedAfterVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.bootstrap(t)

	var logoutErr error
	auth.SetPasswordChangeVerifiedHook(h.service, once(func() {
		concurrently(func() { logoutErr = h.service.Logout(ctx, admin.Token, "192.0.2.1") })
	}))

	if _, err := h.service.ChangePassword(ctx, admin.Token, newPassword, "another good passphrase", "192.0.2.1"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("change after revocation err = %v, want ErrUnauthenticated", err)
	}
	if logoutErr != nil {
		t.Fatalf("concurrent logout: %v", logoutErr)
	}
	if _, err := h.service.Login(ctx, newPassword, "192.0.2.1"); err != nil {
		t.Fatalf("password changed by a revoked session: %v", err)
	}
}

func TestPasswordChangeRejectedWhenPasswordRotatesAfterVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := h.bootstrap(t)
	second, err := h.service.Login(ctx, newPassword, "192.0.2.2")
	if err != nil {
		t.Fatal(err)
	}

	var rotateErr error
	auth.SetPasswordChangeVerifiedHook(h.service, once(func() {
		concurrently(func() {
			_, rotateErr = h.service.ChangePassword(ctx, second.Token, newPassword, "rotated by second session", "192.0.2.2")
		})
	}))

	if _, err := h.service.ChangePassword(ctx, first.Token, newPassword, "rotated by first session", "192.0.2.1"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("stale rotation err = %v, want ErrUnauthenticated", err)
	}
	if rotateErr != nil {
		t.Fatalf("concurrent rotation: %v", rotateErr)
	}
	if _, err := h.service.Login(ctx, "rotated by second session", "192.0.2.3"); err != nil {
		t.Errorf("winning rotation lost: %v", err)
	}
	if _, err := h.service.Login(ctx, "rotated by first session", "192.0.2.4"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("stale rotation persisted: %v", err)
	}
}

func TestBootstrapChangeRejectedWhenAnotherBootstrapCompletesAfterVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.2")
	if err != nil {
		t.Fatal(err)
	}

	var rotateErr error
	auth.SetPasswordChangeVerifiedHook(h.service, once(func() {
		concurrently(func() {
			_, rotateErr = h.service.ChangePassword(ctx, second.Token, auth.DefaultPassword, "set by second bootstrap", "192.0.2.2")
		})
	}))

	if _, err := h.service.ChangePassword(ctx, first.Token, auth.DefaultPassword, "set by first bootstrap", "192.0.2.1"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("stale bootstrap rotation err = %v, want ErrUnauthenticated", err)
	}
	if rotateErr != nil {
		t.Fatalf("concurrent bootstrap rotation: %v", rotateErr)
	}
	if _, err := h.service.Login(ctx, "set by second bootstrap", "192.0.2.3"); err != nil {
		t.Errorf("winning bootstrap rotation lost: %v", err)
	}
}
