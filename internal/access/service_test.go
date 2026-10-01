package access_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type revocation struct {
	terminal string
	grants   []string
	session  int64
	reason   string
}

type listener struct {
	mu     sync.Mutex
	events []revocation
}

func (l *listener) GrantsRevoked(terminalID string, grantIDs []string, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, revocation{terminal: terminalID, grants: append([]string(nil), grantIDs...), reason: reason})
}

func (l *listener) SessionRevoked(sessionID int64, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, revocation{session: sessionID, reason: reason})
}

func (l *listener) all() []revocation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]revocation(nil), l.events...)
}

type harness struct {
	t        *testing.T
	store    *store.Store
	clock    *clock
	listener *listener
	service  *access.Service
}

func newHarness(t *testing.T, configure func(*access.Config)) *harness {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	l := &listener{}
	cfg := access.Config{Store: s, Listener: l, Now: c.Now, SessionTTL: time.Hour}
	if configure != nil {
		configure(&cfg)
	}
	service, err := access.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: s, clock: c, listener: l, service: service}
	for _, id := range []string{"term", "other"} {
		if err := s.CreateTerminalSession(context.Background(), store.TerminalSession{
			PublicID: id, State: store.TerminalRunning, Command: "/bin/sh", Rows: 24, Cols: 80,
			CreatedAt: c.Now(), LastActivityAt: c.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) grant(req access.GrantRequest) access.Invitation {
	h.t.Helper()
	if req.TerminalID == "" {
		req.TerminalID = "term"
	}
	invitation, err := h.service.CreateGrant(context.Background(), req)
	if err != nil {
		h.t.Fatalf("CreateGrant(%+v): %v", req, err)
	}
	return invitation
}

func (h *harness) redeem(token string) access.Session {
	h.t.Helper()
	session, err := h.service.Redeem(context.Background(), token, "198.51.100.7", nil)
	if err != nil {
		h.t.Fatalf("Redeem: %v", err)
	}
	return session
}

func sha(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func TestInvitationTokensAreRandom256BitAndStoredHashed(t *testing.T) {
	h := newHarness(t, nil)
	first := h.grant(access.GrantRequest{Role: store.AccessViewer})
	second := h.grant(access.GrantRequest{Role: store.AccessViewer})

	for _, token := range []string{first.Token, second.Token} {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(raw) < 32 {
			t.Fatalf("token %q decodes to %d bytes (%v), want >= 32", token, len(raw), err)
		}
	}
	if first.Token == second.Token || first.Grant.PublicID == second.Grant.PublicID {
		t.Fatal("tokens or grant IDs repeat")
	}
	if strings.Contains(first.Token, first.Grant.PublicID) || strings.Contains(first.Grant.PublicID, first.Token) {
		t.Fatal("grant ID derived from token")
	}
	if !bytes.Equal(access.HashToken(first.Token), sha(first.Token)) {
		t.Fatal("HashToken is not SHA-256")
	}
	var stored []byte
	if err := h.store.DB().QueryRow(`SELECT token_hash FROM access_grants WHERE public_id = ?`, first.Grant.PublicID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, sha(first.Token)) {
		t.Fatal("stored hash does not match SHA-256 of the token")
	}
}

func TestRedeemCreatesSeparateBoundedSession(t *testing.T) {
	h := newHarness(t, func(c *access.Config) { c.SessionTTL = 10 * time.Minute })
	invitation := h.grant(access.GrantRequest{Role: store.AccessEditor, Label: "pair", TTL: time.Hour})

	session := h.redeem(invitation.Token)
	if session.Token == "" || session.Token == invitation.Token {
		t.Fatalf("session token %q must be new", session.Token)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(session.Token); err != nil || len(raw) < 32 {
		t.Fatalf("session token has %d bytes", len(raw))
	}
	if session.Grant.PublicID != invitation.Grant.PublicID || session.Grant.TerminalID != "term" ||
		session.Grant.Role != store.AccessEditor || session.Grant.RedemptionCount != 1 {
		t.Fatalf("session grant = %+v", session.Grant)
	}
	if want := h.clock.Now().Add(10 * time.Minute); !session.ExpiresAt.Equal(want) {
		t.Fatalf("session expires %v, want %v", session.ExpiresAt, want)
	}
	var count int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM access_sessions WHERE token_hash = ?`, sha(session.Token)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("session hash rows = %d, %v", count, err)
	}

	got, err := h.service.Authenticate(context.Background(), session.Token)
	if err != nil || got.ID != session.ID || got.Token != "" || got.Grant.Role != store.AccessEditor {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}
	if _, err := h.service.Authenticate(context.Background(), invitation.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("invite token as session err = %v, want ErrUnauthenticated", err)
	}

	// A session never outlives its grant.
	short := h.grant(access.GrantRequest{Role: store.AccessViewer, TTL: 2 * time.Minute})
	if s := h.redeem(short.Token); !s.ExpiresAt.Equal(short.Grant.ExpiresAt) {
		t.Fatalf("session expiry %v, want grant expiry %v", s.ExpiresAt, short.Grant.ExpiresAt)
	}
}

func TestRedeemRejectsInvalidExpiredRevokedAndExhaustedGrants(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	for _, token := range []string{"", "not-a-token", strings.Repeat("x", 4096)} {
		if _, err := h.service.Redeem(ctx, token, "", nil); !errors.Is(err, access.ErrInvalidToken) {
			t.Errorf("Redeem(%.10q) err = %v, want ErrInvalidToken", token, err)
		}
	}

	limited := h.grant(access.GrantRequest{Role: store.AccessViewer, MaxRedemptions: 2})
	h.redeem(limited.Token)
	h.redeem(limited.Token)
	if _, err := h.service.Redeem(ctx, limited.Token, "", nil); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("exhausted err = %v", err)
	}

	revoked := h.grant(access.GrantRequest{Role: store.AccessViewer})
	if _, err := h.service.RevokeGrant(ctx, "term", revoked.Grant.PublicID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Redeem(ctx, revoked.Token, "", nil); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("revoked err = %v", err)
	}

	expiring := h.grant(access.GrantRequest{Role: store.AccessViewer, TTL: time.Minute})
	session := h.redeem(expiring.Token)
	h.clock.Advance(time.Minute)
	if _, err := h.service.Redeem(ctx, expiring.Token, "", nil); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("expired err = %v", err)
	}
	if _, err := h.service.Authenticate(ctx, session.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("session of expired grant err = %v", err)
	}
}

func TestSingleUseRedemptionRace(t *testing.T) {
	h := newHarness(t, nil)
	invitation := h.grant(access.GrantRequest{Role: store.AccessEditor, SingleUse: true})
	if !invitation.Grant.SingleUse || invitation.Grant.MaxRedemptions != 1 {
		t.Fatalf("grant = %+v", invitation.Grant)
	}

	const racers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, sessions := 0, map[string]bool{}
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			session, err := h.service.Redeem(context.Background(), invitation.Token, "", nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
				sessions[session.Token] = true
			case !errors.Is(err, access.ErrInvalidToken):
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 || len(sessions) != 1 {
		t.Fatalf("single-use grant redeemed %d times", wins)
	}
	grants, _ := h.service.Grants(context.Background(), "term")
	if len(grants) != 1 || grants[0].RedemptionCount != 1 || grants[0].RedeemedAt.IsZero() {
		t.Fatalf("grant after race = %+v", grants)
	}
}

func TestCreatingEditorReplacesPreviousEditorAndItsSessions(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	oldEditor := h.grant(access.GrantRequest{Role: store.AccessEditor})
	oldSession := h.redeem(oldEditor.Token)
	viewer := h.grant(access.GrantRequest{Role: store.AccessViewer})
	viewerSession := h.redeem(viewer.Token)
	otherEditor := h.grant(access.GrantRequest{TerminalID: "other", Role: store.AccessEditor})

	next := h.grant(access.GrantRequest{Role: store.AccessEditor})
	if len(next.Replaced) != 1 || next.Replaced[0].PublicID != oldEditor.Grant.PublicID ||
		next.Replaced[0].RevokeReason != store.RevokeReasonReplaced {
		t.Fatalf("replaced = %+v", next.Replaced)
	}
	if _, err := h.service.Authenticate(ctx, oldSession.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("replaced editor session err = %v", err)
	}
	if _, err := h.service.Redeem(ctx, oldEditor.Token, "", nil); !errors.Is(err, access.ErrInvalidToken) {
		t.Fatalf("replaced editor invite err = %v", err)
	}
	if _, err := h.service.Authenticate(ctx, viewerSession.Token); err != nil {
		t.Fatalf("viewer session revoked by editor replacement: %v", err)
	}
	h.redeem(next.Token)

	events := h.listener.all()
	if len(events) != 1 || events[0].terminal != "term" || events[0].reason != "replaced" ||
		len(events[0].grants) != 1 || events[0].grants[0] != oldEditor.Grant.PublicID {
		t.Fatalf("listener events = %+v", events)
	}
	grants, _ := h.service.Grants(ctx, "other")
	if len(grants) != 1 || grants[0].PublicID != otherEditor.Grant.PublicID || !grants[0].RevokedAt.IsZero() {
		t.Fatalf("other terminal editor affected: %+v", grants)
	}
}

func TestConcurrentEditorCreationLeavesOneActiveEditor(t *testing.T) {
	h := newHarness(t, nil)
	const racers = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := h.service.CreateGrant(context.Background(), access.GrantRequest{TerminalID: "term", Role: store.AccessEditor}); err != nil {
				t.Errorf("CreateGrant: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	grants, err := h.service.Grants(context.Background(), "term")
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, g := range grants {
		if access.Status(g, h.clock.Now()) == access.StatusActive {
			active++
		}
	}
	replacedEvents := 0
	for _, e := range h.listener.all() {
		replacedEvents += len(e.grants)
	}
	if len(grants) != racers || active != 1 || replacedEvents != racers-1 {
		t.Fatalf("grants=%d active=%d replaced notifications=%d", len(grants), active, replacedEvents)
	}
}

func TestRevokeGrantEndsSessionsAndNotifiesOnce(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	invitation := h.grant(access.GrantRequest{Role: store.AccessViewer})
	session := h.redeem(invitation.Token)

	if _, err := h.service.RevokeGrant(ctx, "other", invitation.Grant.PublicID, ""); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("revoke via other terminal err = %v, want ErrNotFound", err)
	}
	revoked, err := h.service.RevokeGrant(ctx, "term", invitation.Grant.PublicID, "")
	if err != nil || access.Status(revoked, h.clock.Now()) != access.StatusRevoked {
		t.Fatalf("RevokeGrant = %+v, %v", revoked, err)
	}
	if _, err := h.service.Authenticate(ctx, session.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("session after revoke err = %v", err)
	}
	if _, err := h.service.RevokeGrant(ctx, "term", invitation.Grant.PublicID, ""); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	events := h.listener.all()
	if len(events) != 1 || events[0].reason != "revoked" || events[0].grants[0] != invitation.Grant.PublicID {
		t.Fatalf("listener events = %+v", events)
	}
}

func TestLogoutRevokesOnlyThatSession(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	invitation := h.grant(access.GrantRequest{Role: store.AccessViewer})
	first := h.redeem(invitation.Token)
	second := h.redeem(invitation.Token)

	if err := h.service.Logout(ctx, first.Token, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Authenticate(ctx, first.Token); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("logged-out session err = %v", err)
	}
	if _, err := h.service.Authenticate(ctx, second.Token); err != nil {
		t.Fatalf("other session: %v", err)
	}
	if err := h.service.Logout(ctx, first.Token, ""); !errors.Is(err, access.ErrUnauthenticated) {
		t.Fatalf("second logout err = %v", err)
	}
	events := h.listener.all()
	if len(events) != 1 || events[0].session != first.ID || events[0].reason != "logout" {
		t.Fatalf("listener events = %+v", events)
	}
}

func TestCreateGrantValidation(t *testing.T) {
	h := newHarness(t, func(c *access.Config) {
		c.MaxGrantTTL = 24 * time.Hour
		c.MaxRedemptions = 50
	})
	ctx := context.Background()
	for name, req := range map[string]access.GrantRequest{
		"owner role":          {TerminalID: "term", Role: "owner"},
		"empty role":          {TerminalID: "term"},
		"ttl too short":       {TerminalID: "term", Role: store.AccessViewer, TTL: time.Second},
		"ttl too long":        {TerminalID: "term", Role: store.AccessViewer, TTL: 25 * time.Hour},
		"negative ttl":        {TerminalID: "term", Role: store.AccessViewer, TTL: -time.Hour},
		"too many uses":       {TerminalID: "term", Role: store.AccessViewer, MaxRedemptions: 51},
		"negative uses":       {TerminalID: "term", Role: store.AccessViewer, MaxRedemptions: -1},
		"single use and many": {TerminalID: "term", Role: store.AccessViewer, SingleUse: true, MaxRedemptions: 2},
		"long label":          {TerminalID: "term", Role: store.AccessViewer, Label: strings.Repeat("a", access.MaxLabelBytes+1)},
		"control label":       {TerminalID: "term", Role: store.AccessViewer, Label: "a\nb"},
		"invalid utf8 label":  {TerminalID: "term", Role: store.AccessViewer, Label: "\xff"},
	} {
		if _, err := h.service.CreateGrant(ctx, req); !errors.Is(err, access.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	if _, err := h.service.CreateGrant(ctx, access.GrantRequest{TerminalID: "missing", Role: store.AccessViewer}); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("unknown terminal err = %v, want ErrNotFound", err)
	}
	if grants, _ := h.service.Grants(ctx, "term"); len(grants) != 0 {
		t.Fatalf("invalid requests created grants: %+v", grants)
	}

	defaults := h.grant(access.GrantRequest{Role: store.AccessViewer})
	if !defaults.Grant.ExpiresAt.Equal(h.clock.Now().Add(24*time.Hour)) || defaults.Grant.MaxRedemptions != 50 {
		t.Fatalf("defaults = %+v", defaults.Grant)
	}
}

func TestGrantStatus(t *testing.T) {
	now := time.Unix(100, 0)
	base := store.AccessGrant{ExpiresAt: now.Add(time.Minute), MaxRedemptions: 2}
	for want, grant := range map[string]store.AccessGrant{
		access.StatusActive:    base,
		access.StatusExpired:   {ExpiresAt: now, MaxRedemptions: 2},
		access.StatusExhausted: {ExpiresAt: now.Add(time.Minute), MaxRedemptions: 2, RedemptionCount: 2},
		access.StatusRevoked:   {ExpiresAt: now.Add(time.Minute), MaxRedemptions: 2, RevokedAt: now, RevokeReason: store.RevokeReasonRevoked},
		access.StatusReplaced:  {ExpiresAt: now.Add(time.Minute), MaxRedemptions: 2, RevokedAt: now, RevokeReason: store.RevokeReasonReplaced},
	} {
		if got := access.Status(grant, now); got != want {
			t.Errorf("Status(%+v) = %q, want %q", grant, got, want)
		}
	}
}

func TestAuditRecordsLifecycleWithoutSecrets(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	editor := h.grant(access.GrantRequest{Role: store.AccessEditor, RemoteAddr: "192.0.2.9"})
	session := h.redeem(editor.Token)
	replacement := h.grant(access.GrantRequest{Role: store.AccessEditor})
	viewer := h.grant(access.GrantRequest{Role: store.AccessViewer})
	viewerSession := h.redeem(viewer.Token)
	if _, err := h.service.RevokeGrant(ctx, "term", viewer.Grant.PublicID, "192.0.2.9"); err != nil {
		t.Fatal(err)
	}
	third := h.grant(access.GrantRequest{Role: store.AccessViewer})
	thirdSession := h.redeem(third.Token)
	if err := h.service.Logout(ctx, thirdSession.Token, "198.51.100.7"); err != nil {
		t.Fatal(err)
	}

	events, err := h.store.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, event := range events {
		types = append(types, event.Type)
	}
	want := []string{
		"access.grant.created", "access.grant.redeemed", "access.grant.replaced", "access.grant.created",
		"access.grant.created", "access.grant.redeemed", "access.grant.revoked",
		"access.grant.created", "access.grant.redeemed", "access.logout",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("audit types = %v, want %v", types, want)
	}
	if events[0].Details["grantId"] != editor.Grant.PublicID || events[0].Details["role"] != "editor" ||
		events[0].Details["terminalId"] != "term" || events[0].RemoteAddr != "192.0.2.9" {
		t.Fatalf("created event = %+v", events[0])
	}
	if events[2].Details["grantId"] != editor.Grant.PublicID || events[2].Details["replacedBy"] != replacement.Grant.PublicID {
		t.Fatalf("replaced event = %+v", events[2])
	}

	secrets := []string{}
	for _, token := range []string{editor.Token, session.Token, replacement.Token, viewer.Token, viewerSession.Token, third.Token, thirdSession.Token} {
		secrets = append(secrets, token, hex.EncodeToString(sha(token)), base64.RawURLEncoding.EncodeToString(sha(token)),
			access.CSRFToken(token))
	}
	for _, event := range events {
		blob := event.Type + event.RemoteAddr
		for k, v := range event.Details {
			blob += k + v
		}
		for _, secret := range secrets {
			if strings.Contains(blob, secret) {
				t.Fatalf("audit event %s contains a capability secret", event.Type)
			}
		}
	}
}

func TestCSRFTokenIsBoundToSession(t *testing.T) {
	csrf := access.CSRFToken("session-a")
	if csrf == "" || !access.VerifyCSRFToken("session-a", csrf) {
		t.Fatal("CSRF token does not verify for its session")
	}
	if access.VerifyCSRFToken("session-b", csrf) || access.VerifyCSRFToken("session-a", "") || access.VerifyCSRFToken("", csrf) {
		t.Fatal("CSRF token verifies for the wrong session")
	}
}
