package auth

import (
	"net"
	"sync"
	"time"
)

// ThrottleConfig configures a fixed-window per-key attempt limiter.
type ThrottleConfig struct {
	Limit      int
	Window     time.Duration
	MaxEntries int
	Now        func() time.Time
}

// Throttle limits attempts per key using bounded memory. When full, it drops
// expired windows first and then the oldest window.
type Throttle struct {
	mu      sync.Mutex
	config  ThrottleConfig
	entries map[string]*throttleEntry
}

type throttleEntry struct {
	start  time.Time
	count  int
	denied bool
}

// Decision is the outcome of one attempt. FirstDenial is set only on the
// first denied attempt of a key's window, so callers can record one audit
// event per window instead of one per attempt. RetryAfter is set on denials
// and is the time left in the key's window.
type Decision struct {
	Allowed     bool
	FirstDenial bool
	RetryAfter  time.Duration
}

// NewThrottle returns a Throttle; zero fields receive safe defaults.
func NewThrottle(config ThrottleConfig) *Throttle {
	if config.Limit <= 0 {
		config.Limit = 10
	}
	if config.Window <= 0 {
		config.Window = time.Minute
	}
	if config.MaxEntries <= 0 {
		config.MaxEntries = 4096
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Throttle{config: config, entries: make(map[string]*throttleEntry)}
}

// Allow records an attempt for key and reports whether it is within the limit.
func (t *Throttle) Allow(key string) bool { return t.Check(key).Allowed }

// Check records an attempt for key and reports the decision.
func (t *Throttle) Check(key string) Decision {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.config.Now()
	entry, ok := t.entries[key]
	if ok && now.Sub(entry.start) >= t.config.Window {
		entry.start, entry.count, entry.denied = now, 0, false
	}
	if !ok {
		if len(t.entries) >= t.config.MaxEntries {
			t.evict(now)
		}
		entry = &throttleEntry{start: now}
		t.entries[key] = entry
	}
	if entry.count >= t.config.Limit {
		first := !entry.denied
		entry.denied = true
		return Decision{FirstDenial: first, RetryAfter: entry.start.Add(t.config.Window).Sub(now)}
	}
	entry.count++
	return Decision{Allowed: true}
}

// Len reports the number of tracked keys.
func (t *Throttle) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

func (t *Throttle) evict(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for key, entry := range t.entries {
		if now.Sub(entry.start) >= t.config.Window {
			delete(t.entries, key)
			continue
		}
		if oldestKey == "" || entry.start.Before(oldest) {
			oldestKey, oldest = key, entry.start
		}
	}
	if len(t.entries) >= t.config.MaxEntries && oldestKey != "" {
		delete(t.entries, oldestKey)
	}
}

// ipv6PrefixBits is how much of an IPv6 address identifies one client. A
// single host commonly controls a whole /64.
const ipv6PrefixBits = 64

// ThrottleKey normalizes a remote IP for throttling: IPv4-mapped IPv6
// addresses become IPv4, and other IPv6 addresses are reduced to their /64
// prefix so one host cannot multiply its budget by rotating addresses.
// Anything that is not an IP is returned unchanged.
func ThrottleKey(remoteAddr string) string {
	ip := net.ParseIP(remoteAddr)
	if ip == nil {
		return remoteAddr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(ipv6PrefixBits, 128)).String() + "/64"
}
