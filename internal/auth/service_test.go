package auth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const newPassword = "a much better passphrase"

type harness struct {
	service *auth.Service
	store   *store.Store
	clock   *fakeClock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	service := auth.NewService(auth.ServiceConfig{
		Store:            s,
		PasswordParams:   fastParams,
		SessionTTL:       time.Hour,
		BootstrapTTL:     10 * time.Minute,
		Throttle:         auth.NewThrottle(auth.ThrottleConfig{Limit: 5, Window: time.Minute, MaxEntries: 16, Now: clock.Now}),
		PasswordThrottle: auth.NewThrottle(auth.ThrottleConfig{Limit: 3, Window: time.Minute, MaxEntries: 16, Now: clock.Now}),
		Now:              clock.Now,
	})
	return &harness{service: service, store: s, clock: clock}
}

func (h *harness) bootstrap(t *testing.T) auth.Session {
	t.Helper()
	ctx := context.Background()
	bootstrap, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatalf("bootstrap login: %v", err)
	}
	session, err := h.service.ChangePassword(ctx, bootstrap.Token, auth.DefaultPassword, newPassword, "192.0.2.1")
	if err != nil {
		t.Fatalf("bootstrap change password: %v", err)
	}
	return session
}

func TestBootstrapLoginWithDefaultPassword(t *testing.T) {
	h := newHarness(t)

	session, err := h.service.Login(context.Background(), "CHANGEME", "192.0.2.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session.Kind != store.SessionKindBootstrap || !session.PasswordChangeRequired() {
		t.Errorf("Kind = %q, want bootstrap requiring password change", session.Kind)
	}
	if len(session.Token) < 40 {
		t.Errorf("token %q is too short to carry 256 bits", session.Token)
	}
	if want := h.clock.now.Add(10 * time.Minute); !session.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", session.ExpiresAt, want)
	}
}

func TestBootstrapLoginRejectsOtherPasswords(t *testing.T) {
	h := newHarness(t)
	for _, password := range []string{"", "changeme", "CHANGEME ", newPassword} {
		if _, err := h.service.Login(context.Background(), password, "192.0.2.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("Login(%q) err = %v, want ErrInvalidCredentials", password, err)
		}
	}
}

func TestSessionsAreIssuedWithRandomTokens(t *testing.T) {
	h := newHarness(t)
	first, err := h.service.Login(context.Background(), auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Login(context.Background(), auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("two logins returned the same token")
	}
}

func TestBootstrapPasswordChangeInitializesAdmin(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bootstrap, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	session, err := h.service.ChangePassword(ctx, bootstrap.Token, auth.DefaultPassword, newPassword, "192.0.2.1")
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if session.Kind != store.SessionKindAdmin || session.PasswordChangeRequired() {
		t.Errorf("new session Kind = %q, want admin", session.Kind)
	}
	if want := h.clock.now.Add(time.Hour); !session.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", session.ExpiresAt, want)
	}

	credential, err := h.store.AdminCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(credential.PasswordHash, "$argon2id$") {
		t.Errorf("stored hash = %q, want argon2id encoding", credential.PasswordHash)
	}

	if _, err := h.service.Authenticate(ctx, bootstrap.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("bootstrap token after change err = %v, want ErrUnauthenticated", err)
	}
	if _, err := h.service.Authenticate(ctx, session.Token); err != nil {
		t.Errorf("new admin session rejected: %v", err)
	}
}

func TestDefaultPasswordRejectedAfterInitialization(t *testing.T) {
	h := newHarness(t)
	h.bootstrap(t)

	if _, err := h.service.Login(context.Background(), auth.DefaultPassword, "192.0.2.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("Login(CHANGEME) after init err = %v, want ErrInvalidCredentials", err)
	}
}

func TestLoginAfterInitialization(t *testing.T) {
	h := newHarness(t)
	h.bootstrap(t)

	session, err := h.service.Login(context.Background(), newPassword, "192.0.2.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session.Kind != store.SessionKindAdmin || session.PasswordChangeRequired() {
		t.Errorf("Kind = %q, want admin", session.Kind)
	}
	if _, err := h.service.Login(context.Background(), newPassword+"x", "192.0.2.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong password err = %v, want ErrInvalidCredentials", err)
	}
}

func TestBootstrapPasswordChangeRequiresDefaultCurrentPassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bootstrap, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.service.ChangePassword(ctx, bootstrap.Token, "WRONG", newPassword, "192.0.2.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
	if _, err := h.store.AdminCredential(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("credential stored despite wrong current password: %v", err)
	}
}

func TestPasswordChangeRequiresValidSession(t *testing.T) {
	h := newHarness(t)
	for _, token := range []string{"", "forged-token"} {
		if _, err := h.service.ChangePassword(context.Background(), token, auth.DefaultPassword, newPassword, "192.0.2.1"); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("ChangePassword(token=%q) err = %v, want ErrUnauthenticated", token, err)
		}
	}
}

func TestPasswordChangeRejectsWeakPasswords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bootstrap, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []string{"", "short", "elevenchars", "CHANGEME", strings.Repeat("x", 1025)} {
		if _, err := h.service.ChangePassword(ctx, bootstrap.Token, auth.DefaultPassword, candidate, "192.0.2.1"); !errors.Is(err, auth.ErrInvalidNewPassword) {
			t.Errorf("ChangePassword(new=%d chars) err = %v, want ErrInvalidNewPassword", len(candidate), err)
		}
	}
	if _, err := h.service.ChangePassword(ctx, bootstrap.Token, auth.DefaultPassword, "twelve chars", "192.0.2.1"); err != nil {
		t.Errorf("12-character password rejected: %v", err)
	}
}

func TestAdminPasswordChangeRevokesPriorSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := h.bootstrap(t)
	second, err := h.service.Login(ctx, newPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.service.ChangePassword(ctx, first.Token, "wrong current", "another good passphrase", "192.0.2.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong current password err = %v, want ErrInvalidCredentials", err)
	}

	fresh, err := h.service.ChangePassword(ctx, first.Token, newPassword, "another good passphrase", "192.0.2.1")
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	for name, token := range map[string]string{"first": first.Token, "second": second.Token} {
		if _, err := h.service.Authenticate(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s session after change err = %v, want ErrUnauthenticated", name, err)
		}
	}
	if _, err := h.service.Authenticate(ctx, fresh.Token); err != nil {
		t.Errorf("fresh session rejected: %v", err)
	}
	if _, err := h.service.Login(ctx, newPassword, "192.0.2.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("old password still accepted: %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bootstrap, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	h.clock.Advance(10*time.Minute - time.Nanosecond)
	if _, err := h.service.Authenticate(ctx, bootstrap.Token); err != nil {
		t.Fatalf("bootstrap session rejected before expiry: %v", err)
	}
	h.clock.Advance(time.Nanosecond)
	if _, err := h.service.Authenticate(ctx, bootstrap.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expired bootstrap session err = %v, want ErrUnauthenticated", err)
	}
	if _, err := h.service.ChangePassword(ctx, bootstrap.Token, auth.DefaultPassword, newPassword, "192.0.2.1"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("password change with expired bootstrap err = %v, want ErrUnauthenticated", err)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session := h.bootstrap(t)

	if err := h.service.Logout(ctx, session.Token, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Authenticate(ctx, session.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("session after logout err = %v, want ErrUnauthenticated", err)
	}
}

func TestLoginThrottlePerIP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for range 5 {
		_, _ = h.service.Login(ctx, "wrong", "192.0.2.1")
	}
	if _, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1"); !errors.Is(err, auth.ErrThrottled) {
		t.Fatalf("sixth attempt err = %v, want ErrThrottled", err)
	}
	if _, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.2"); err != nil {
		t.Fatalf("other IP throttled: %v", err)
	}
	h.clock.Advance(time.Minute)
	if _, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1"); err != nil {
		t.Fatalf("attempt after window: %v", err)
	}
}

func TestCSRFTokenIsSessionBound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	csrf := auth.CSRFToken(first.Token)
	if csrf == "" || csrf == first.Token {
		t.Fatalf("CSRFToken = %q, want a distinct non-empty value", csrf)
	}
	if !auth.VerifyCSRFToken(first.Token, csrf) {
		t.Error("CSRF token rejected for its own session")
	}
	if auth.VerifyCSRFToken(second.Token, csrf) {
		t.Error("CSRF token accepted for a different session")
	}
	if auth.VerifyCSRFToken(first.Token, "") {
		t.Error("empty CSRF token accepted")
	}
}

func TestAuditTrailRecordsEventsWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	_, _ = h.service.Login(ctx, "guess-one", "192.0.2.1")
	bootstrap, err := h.service.Login(ctx, auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := h.service.ChangePassword(ctx, bootstrap.Token, auth.DefaultPassword, newPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.Logout(ctx, admin.Token, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		_, _ = h.service.Login(ctx, "guess-two", "192.0.2.9")
	}

	events, err := h.store.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, event := range events {
		types = append(types, event.Type)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{
		"admin.login.failure",
		"admin.login.success",
		"admin.password.changed",
		"admin.logout",
		"admin.login.throttled",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit trail %v missing %q", types, want)
		}
	}

	secrets := []string{"guess-one", "guess-two", auth.DefaultPassword, newPassword, bootstrap.Token, admin.Token}
	assertDatabaseFreeOf(t, h.store, secrets)
}

func TestSessionsPersistOnlyTokenHashes(t *testing.T) {
	h := newHarness(t)
	session, err := h.service.Login(context.Background(), auth.DefaultPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := h.store.DB().QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(session.Token))
	if string(stored) != string(sum[:]) {
		t.Fatal("stored token hash is not SHA-256 of the issued token")
	}
	assertDatabaseFreeOf(t, h.store, []string{session.Token, auth.CSRFToken(session.Token)})
}

func assertDatabaseFreeOf(t *testing.T, s *store.Store, secrets []string) {
	t.Helper()
	tables, err := s.DB().Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	tables.Close()

	for _, table := range names {
		rows, err := s.DB().Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				var text string
				switch v := value.(type) {
				case string:
					text = v
				case []byte:
					text = string(v)
				default:
					continue
				}
				for _, secret := range secrets {
					if secret != "" && strings.Contains(text, secret) {
						t.Errorf("%s.%s contains secret %q", table, columns[i], secret)
					}
				}
			}
		}
		rows.Close()
	}
}
