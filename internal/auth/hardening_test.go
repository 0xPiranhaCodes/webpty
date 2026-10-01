package auth_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func TestThrottleKeyGroupsIPv6ByPrefix(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2:ffff::1", true},
		{"2001:db8:1:2::1", "2001:db8:1:3::1", false},
		{"::ffff:192.0.2.1", "192.0.2.1", true},
		{"192.0.2.1", "192.0.2.2", false},
		{"not-an-ip", "not-an-ip", true},
	}
	for _, c := range cases {
		if got := auth.ThrottleKey(c.a) == auth.ThrottleKey(c.b); got != c.same {
			t.Errorf("ThrottleKey(%q) == ThrottleKey(%q) is %v, want %v (%q, %q)",
				c.a, c.b, got, c.same, auth.ThrottleKey(c.a), auth.ThrottleKey(c.b))
		}
	}
}

func TestThrottleCheckReportsFirstDenialPerWindow(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	throttle := auth.NewThrottle(auth.ThrottleConfig{Limit: 1, Window: time.Minute, MaxEntries: 4, Now: clock.Now})

	if d := throttle.Check("k"); !d.Allowed || d.FirstDenial {
		t.Fatalf("first attempt = %+v, want allowed", d)
	}
	if d := throttle.Check("k"); d.Allowed || !d.FirstDenial {
		t.Fatalf("second attempt = %+v, want first denial", d)
	}
	if d := throttle.Check("k"); d.Allowed || d.FirstDenial {
		t.Fatalf("third attempt = %+v, want repeated denial", d)
	}
	clock.Advance(time.Minute)
	throttle.Check("k")
	if d := throttle.Check("k"); d.Allowed || !d.FirstDenial {
		t.Fatalf("denial in next window = %+v, want first denial", d)
	}
}

func newHardeningService(t *testing.T, build func(now func() time.Time) auth.ServiceConfig) (*auth.Service, *store.Store, *fakeClock) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	now := func() time.Time { return clock.Now() }
	cfg := build(now)
	cfg.Store, cfg.PasswordParams, cfg.Now = s, fastParams, now
	return auth.NewService(cfg), s, clock
}

func countAudit(t *testing.T, s *store.Store, eventType string) int {
	t.Helper()
	events, err := s.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

// An attacker holding a session must not reset its password-change budget
// by rotating source addresses until the session's entry is evicted.
func TestPasswordSessionThrottleSurvivesIPChurn(t *testing.T) {
	service, _, clock := newHardeningService(t, func(now func() time.Time) auth.ServiceConfig {
		return auth.ServiceConfig{
			PasswordThrottle:        auth.NewThrottle(auth.ThrottleConfig{Limit: 3, Window: time.Hour, MaxEntries: 4, Now: now}),
			PasswordSessionThrottle: auth.NewThrottle(auth.ThrottleConfig{Limit: 3, Window: time.Hour, MaxEntries: 4, Now: now}),
		}
	})
	ctx := context.Background()
	bootstrap, err := service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	for i := range 40 {
		clock.Advance(time.Millisecond)
		_, err := service.ChangePassword(ctx, bootstrap.Token, "wrong password!", newPassword, fmt.Sprintf("198.51.100.%d", i))
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			verified++
		case errors.Is(err, auth.ErrThrottled):
		default:
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if verified != 3 {
		t.Fatalf("password checks reached = %d across rotating IPs, want 3", verified)
	}
}

func TestThrottledLoginIsAuditedOncePerWindow(t *testing.T) {
	service, s, clock := newHardeningService(t, func(now func() time.Time) auth.ServiceConfig {
		return auth.ServiceConfig{Throttle: auth.NewThrottle(auth.ThrottleConfig{Limit: 2, Window: time.Minute, Now: now})}
	})
	ctx := context.Background()
	for range 10 {
		_, _ = service.Login(ctx, "wrong", "192.0.2.9")
	}
	if n := countAudit(t, s, "admin.login.throttled"); n != 1 {
		t.Fatalf("throttled audit events = %d after 8 throttled logins, want 1", n)
	}
	clock.Advance(time.Minute)
	for range 5 {
		_, _ = service.Login(ctx, "wrong", "192.0.2.9")
	}
	if n := countAudit(t, s, "admin.login.throttled"); n != 2 {
		t.Fatalf("throttled audit events = %d after a second window, want 2", n)
	}
}

func TestLoginThrottleGroupsIPv6Prefix(t *testing.T) {
	service, _, _ := newHardeningService(t, func(now func() time.Time) auth.ServiceConfig {
		return auth.ServiceConfig{Throttle: auth.NewThrottle(auth.ThrottleConfig{Limit: 3, Window: time.Minute, Now: now})}
	})
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := service.Login(ctx, "wrong", fmt.Sprintf("2001:db8:5:6::%x", i)); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := service.Login(ctx, "wrong", "2001:db8:5:6:aaaa::1"); !errors.Is(err, auth.ErrThrottled) {
		t.Fatalf("fourth attempt from the same /64 = %v, want ErrThrottled", err)
	}
	if _, err := service.Login(ctx, "wrong", "2001:db8:5:7::1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("attempt from another /64 = %v, want ErrInvalidCredentials", err)
	}
}

func TestDeleteExpiredSessionsSweepsOnlyExpired(t *testing.T) {
	service, s, clock := newHardeningService(t, func(func() time.Time) auth.ServiceConfig {
		return auth.ServiceConfig{SessionTTL: time.Hour, BootstrapTTL: time.Minute}
	})
	ctx := context.Background()
	old, err := service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	fresh, err := service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := s.DeleteExpiredSessions(ctx, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM admin_sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("admin sessions left = %d, want 1", rows)
	}
	if _, err := service.Authenticate(ctx, fresh.Token); err != nil {
		t.Fatalf("fresh session: %v", err)
	}
	if _, err := service.Authenticate(ctx, old.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expired session: %v", err)
	}
}
