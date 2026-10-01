package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// DefaultPassword is the only password accepted before the administrator is initialized.
const DefaultPassword = "CHANGEME"

const (
	MinPasswordLength = 12
	maxPasswordBytes  = 1024
	tokenBytes        = 32
	csrfDomain        = "webpty-csrf-v1\x00"
)

var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrUnauthenticated    = errors.New("auth: unauthenticated")
	ErrThrottled          = errors.New("auth: too many attempts")
	ErrInvalidNewPassword = errors.New("auth: new password does not meet requirements")
)

// ServiceConfig wires a Service.
type ServiceConfig struct {
	Store          *store.Store
	PasswordParams PasswordParams
	SessionTTL     time.Duration
	BootstrapTTL   time.Duration
	Throttle       *Throttle
	// PasswordThrottle limits password-change attempts per session and per IP.
	PasswordThrottle        *Throttle
	PasswordSessionThrottle *Throttle
	Now                     func() time.Time
}

// Service authenticates the single administrator.
type Service struct {
	config ServiceConfig
	hooks  hooks
}

// hooks let tests interleave concurrent operations at fixed points.
type hooks struct {
	loginVerified          func()
	passwordChangeVerify   func()
	passwordChangeVerified func()
}

func run(hook func()) {
	if hook != nil {
		hook()
	}
}

// Session describes an authenticated session. Token is the raw bearer token
// and is set only on sessions returned by Login and ChangePassword.
type Session struct {
	Token     string
	Kind      store.SessionKind
	ExpiresAt time.Time
}

// PasswordChangeRequired reports whether the session is a bootstrap session.
func (s Session) PasswordChangeRequired() bool { return s.Kind == store.SessionKindBootstrap }

// NewService returns a Service; zero fields receive defaults.
func NewService(config ServiceConfig) *Service {
	if config.PasswordParams == (PasswordParams{}) {
		config.PasswordParams = DefaultPasswordParams
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = 12 * time.Hour
	}
	if config.BootstrapTTL <= 0 {
		config.BootstrapTTL = 10 * time.Minute
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Throttle == nil {
		config.Throttle = NewThrottle(ThrottleConfig{Now: config.Now})
	}
	if config.PasswordThrottle == nil {
		config.PasswordThrottle = NewThrottle(ThrottleConfig{Limit: 5, Window: 5 * time.Minute, Now: config.Now})
	}
	if config.PasswordSessionThrottle == nil {
		ip := config.PasswordThrottle.config
		config.PasswordSessionThrottle = NewThrottle(ThrottleConfig{Limit: ip.Limit, Window: ip.Window, Now: ip.Now})
	}
	return &Service{config: config}
}

// Now returns the service clock's current time.
func (s *Service) Now() time.Time { return s.config.Now() }

// ThrottledError reports a throttled attempt. It matches ErrThrottled, and
// RetryAfter is how long until another attempt can be accepted.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string        { return ErrThrottled.Error() }
func (e *ThrottledError) Is(target error) bool { return target == ErrThrottled }

// Login verifies password and issues a bootstrap or admin session. The
// session is persisted only if the credential it was verified against is
// still current.
func (s *Service) Login(ctx context.Context, password, remoteAddr string) (Session, error) {
	now := s.config.Now()
	if decision := s.config.Throttle.Check(ThrottleKey(remoteAddr)); !decision.Allowed {
		if decision.FirstDenial {
			if err := s.audit(ctx, now, "admin.login.throttled", remoteAddr, nil); err != nil {
				return Session{}, err
			}
		}
		return Session{}, &ThrottledError{RetryAfter: decision.RetryAfter}
	}

	snapshot, err := s.snapshotCredential(ctx)
	if err != nil {
		return Session{}, err
	}
	valid, err := snapshot.verify(password)
	if err != nil {
		return Session{}, err
	}
	run(s.hooks.loginVerified)
	if !valid {
		return Session{}, s.loginFailure(ctx, now, remoteAddr, "invalid_password")
	}

	session, err := s.issue(snapshot.sessionKind(), now)
	if err != nil {
		return Session{}, err
	}
	err = s.config.Store.WithTx(ctx, func(tx *store.Tx) error {
		if err := snapshot.stillCurrent(ctx, tx); err != nil {
			return err
		}
		return tx.CreateAdminSession(ctx, s.record(session, now))
	})
	if errors.Is(err, errCredentialChanged) {
		return Session{}, s.loginFailure(ctx, now, remoteAddr, "credential_changed")
	}
	if err != nil {
		return Session{}, err
	}
	if err := s.audit(ctx, now, "admin.login.success", remoteAddr, map[string]string{"sessionKind": string(session.Kind)}); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Service) loginFailure(ctx context.Context, now time.Time, remoteAddr, reason string) error {
	if err := s.audit(ctx, now, "admin.login.failure", remoteAddr, map[string]string{"reason": reason}); err != nil {
		return err
	}
	return ErrInvalidCredentials
}

// Authenticate resolves a raw bearer token to a live session.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrUnauthenticated
	}
	now := s.config.Now()
	record, err := s.config.Store.AdminSession(ctx, hashToken(token), now)
	if errors.Is(err, store.ErrNotFound) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	if !record.ExpiresAt.After(now) {
		return Session{}, ErrUnauthenticated
	}
	if record.Kind == store.SessionKindBootstrap {
		if _, err := s.config.Store.AdminCredential(ctx); !errors.Is(err, store.ErrNotFound) {
			return Session{}, ErrUnauthenticated
		}
	}
	return Session{Kind: record.Kind, ExpiresAt: record.ExpiresAt}, nil
}

// ChangePassword rotates the administrator password, revokes every existing
// session, and returns a fresh admin session. The current password is
// verified against a credential snapshot outside any write transaction; the
// rotation commits only if that credential and the caller's session are
// still current.
func (s *Service) ChangePassword(ctx context.Context, token, currentPassword, newPassword, remoteAddr string) (Session, error) {
	current, err := s.Authenticate(ctx, token)
	if err != nil {
		return Session{}, err
	}
	if !validNewPassword(newPassword) {
		return Session{}, ErrInvalidNewPassword
	}

	now := s.config.Now()
	tokenHash := hashToken(token)
	bySession := s.config.PasswordSessionThrottle.Check(hex.EncodeToString(tokenHash))
	byIP := s.config.PasswordThrottle.Check(ThrottleKey(remoteAddr))
	if !bySession.Allowed || !byIP.Allowed {
		if bySession.FirstDenial || byIP.FirstDenial {
			if err := s.audit(ctx, now, "admin.password.throttled", remoteAddr, nil); err != nil {
				return Session{}, err
			}
		}
		return Session{}, &ThrottledError{RetryAfter: max(bySession.RetryAfter, byIP.RetryAfter)}
	}

	snapshot, err := s.snapshotCredential(ctx)
	if err != nil {
		return Session{}, err
	}
	if snapshot.sessionKind() != current.Kind {
		return Session{}, ErrUnauthenticated
	}
	run(s.hooks.passwordChangeVerify)
	valid, err := snapshot.verify(currentPassword)
	if err != nil {
		return Session{}, err
	}
	if !valid {
		if err := s.audit(ctx, now, "admin.password.failure", remoteAddr, map[string]string{"reason": "invalid_current_password"}); err != nil {
			return Session{}, err
		}
		return Session{}, ErrInvalidCredentials
	}
	run(s.hooks.passwordChangeVerified)

	encoded, err := HashPassword(newPassword, s.config.PasswordParams)
	if err != nil {
		return Session{}, err
	}
	fresh, err := s.issue(store.SessionKindAdmin, now)
	if err != nil {
		return Session{}, err
	}
	err = s.config.Store.WithTx(ctx, func(tx *store.Tx) error {
		live, err := tx.AdminSession(ctx, tokenHash, now)
		if errors.Is(err, store.ErrNotFound) || (err == nil && live.Kind != current.Kind) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		if err := snapshot.stillCurrent(ctx, tx); err != nil {
			return err
		}
		if err := tx.SetAdminCredential(ctx, encoded, now); err != nil {
			return err
		}
		if err := tx.DeleteAllAdminSessions(ctx); err != nil {
			return err
		}
		return tx.CreateAdminSession(ctx, s.record(fresh, now))
	})
	if errors.Is(err, errCredentialChanged) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	if err := s.audit(ctx, now, "admin.password.changed", remoteAddr, map[string]string{"fromSessionKind": string(current.Kind)}); err != nil {
		return Session{}, err
	}
	return fresh, nil
}

// credentialSnapshot is the credential state a password was verified against.
type credentialSnapshot struct {
	bootstrap  bool
	credential store.AdminCredential
}

var errCredentialChanged = errors.New("auth: credential changed concurrently")

func (s *Service) snapshotCredential(ctx context.Context) (credentialSnapshot, error) {
	credential, err := s.config.Store.AdminCredential(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return credentialSnapshot{bootstrap: true}, nil
	}
	if err != nil {
		return credentialSnapshot{}, err
	}
	return credentialSnapshot{credential: credential}, nil
}

func (c credentialSnapshot) sessionKind() store.SessionKind {
	if c.bootstrap {
		return store.SessionKindBootstrap
	}
	return store.SessionKindAdmin
}

func (c credentialSnapshot) verify(password string) (bool, error) {
	if c.bootstrap {
		return isDefaultPassword(password), nil
	}
	ok, err := VerifyPassword(c.credential.PasswordHash, password)
	if err != nil {
		return false, fmt.Errorf("verify admin password: %w", err)
	}
	return ok, nil
}

// stillCurrent returns errCredentialChanged unless the credential read in tx
// matches the snapshot.
func (c credentialSnapshot) stillCurrent(ctx context.Context, tx *store.Tx) error {
	credential, err := tx.AdminCredential(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if c.bootstrap {
			return nil
		}
		return errCredentialChanged
	case err != nil:
		return err
	case c.bootstrap || credential.Version != c.credential.Version || credential.PasswordHash != c.credential.PasswordHash:
		return errCredentialChanged
	}
	return nil
}

// Logout revokes the session identified by token.
func (s *Service) Logout(ctx context.Context, token, remoteAddr string) error {
	if token == "" {
		return ErrUnauthenticated
	}
	if err := s.config.Store.DeleteAdminSession(ctx, hashToken(token)); err != nil {
		return err
	}
	return s.audit(ctx, s.config.Now(), "admin.logout", remoteAddr, nil)
}

// CSRFToken derives the CSRF token bound to a session token. It is not
// persisted and cannot be derived from the stored session hash.
func CSRFToken(sessionToken string) string {
	sum := sha256.Sum256([]byte(csrfDomain + sessionToken))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyCSRFToken reports whether csrf is bound to sessionToken.
func VerifyCSRFToken(sessionToken, csrf string) bool {
	if sessionToken == "" || csrf == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(CSRFToken(sessionToken)), []byte(csrf)) == 1
}

func (s *Service) issue(kind store.SessionKind, now time.Time) (Session, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, fmt.Errorf("generate session token: %w", err)
	}
	ttl := s.config.SessionTTL
	if kind == store.SessionKindBootstrap {
		ttl = s.config.BootstrapTTL
	}
	return Session{
		Token:     base64.RawURLEncoding.EncodeToString(raw),
		Kind:      kind,
		ExpiresAt: now.Add(ttl),
	}, nil
}

func (s *Service) record(session Session, now time.Time) store.AdminSession {
	return store.AdminSession{
		TokenHash: hashToken(session.Token),
		Kind:      session.Kind,
		CreatedAt: now,
		ExpiresAt: session.ExpiresAt,
	}
}

func (s *Service) audit(ctx context.Context, now time.Time, eventType, remoteAddr string, details map[string]string) error {
	return s.config.Store.AppendAuditEvent(ctx, store.AuditEvent{
		OccurredAt: now,
		Type:       eventType,
		RemoteAddr: remoteAddr,
		Details:    details,
	})
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func isDefaultPassword(password string) bool {
	return subtle.ConstantTimeCompare([]byte(password), []byte(DefaultPassword)) == 1
}

func validNewPassword(password string) bool {
	return len(password) <= maxPasswordBytes &&
		utf8.ValidString(password) &&
		utf8.RuneCountInString(password) >= MinPasswordLength &&
		password != DefaultPassword
}
