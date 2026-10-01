package auth_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestThrottleAllowsUpToLimitPerWindow(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	throttle := auth.NewThrottle(auth.ThrottleConfig{Limit: 3, Window: time.Minute, MaxEntries: 10, Now: clock.Now})

	for i := range 3 {
		if !throttle.Allow("192.0.2.1") {
			t.Fatalf("attempt %d denied, want allowed", i+1)
		}
	}
	if throttle.Allow("192.0.2.1") {
		t.Fatal("attempt 4 allowed, want denied")
	}
	if !throttle.Allow("192.0.2.2") {
		t.Fatal("other IP denied, want allowed")
	}

	clock.Advance(time.Minute)
	if !throttle.Allow("192.0.2.1") {
		t.Fatal("attempt after window denied, want allowed")
	}
}

func TestThrottleDenialReportsTimeLeftInWindow(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	throttle := auth.NewThrottle(auth.ThrottleConfig{Limit: 1, Window: time.Minute, Now: clock.Now})

	if decision := throttle.Check("192.0.2.1"); !decision.Allowed || decision.RetryAfter != 0 {
		t.Fatalf("first attempt = %+v, want allowed with no wait", decision)
	}
	clock.Advance(45 * time.Second)
	if decision := throttle.Check("192.0.2.1"); decision.Allowed || decision.RetryAfter != 15*time.Second {
		t.Fatalf("denied attempt = %+v, want RetryAfter 15s", decision)
	}
	clock.Advance(15 * time.Second)
	if !throttle.Allow("192.0.2.1") {
		t.Fatal("attempt when RetryAfter elapsed denied, want allowed")
	}
}

func TestThrottleBoundsMemory(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	throttle := auth.NewThrottle(auth.ThrottleConfig{Limit: 1, Window: time.Hour, MaxEntries: 4, Now: clock.Now})

	for i := range 100 {
		throttle.Allow(fmt.Sprintf("198.51.100.%d", i))
		clock.Advance(time.Millisecond)
		if n := throttle.Len(); n > 4 {
			t.Fatalf("tracked entries = %d after %d IPs, want <= 4", n, i+1)
		}
	}
}

func TestThrottleEvictsOldestWhenFull(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	throttle := auth.NewThrottle(auth.ThrottleConfig{Limit: 1, Window: time.Hour, MaxEntries: 2, Now: clock.Now})

	throttle.Allow("a")
	clock.Advance(time.Second)
	throttle.Allow("b")
	clock.Advance(time.Second)
	throttle.Allow("c")

	if throttle.Allow("b") {
		t.Fatal("recent entry b was evicted instead of the oldest")
	}
	if throttle.Allow("c") {
		t.Fatal("newest entry c was not tracked")
	}
}
