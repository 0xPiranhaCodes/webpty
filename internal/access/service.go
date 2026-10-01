// Package access issues and validates capability grants: invitation tokens
// that let a guest edit or view one terminal, and the access sessions they
// are redeemed for.
package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const (
	tokenBytes   = 32
	grantIDBytes = 12
	csrfDomain   = "webpty-access-csrf-v1\x00"

	// MinGrantTTL is the shortest configurable grant lifetime.
	MinGrantTTL = time.Minute
	// MaxLabelBytes bounds a grant's label.
	MaxLabelBytes = 64
)

// Grant statuses reported by Status.
const (
	StatusActive    = "active"
	StatusExpired   = "expired"
	StatusExhausted = "exhausted"
	StatusRevoked   = "revoked"
	StatusReplaced  = "replaced"
)

// Revocation reasons passed to a Listener.
const (
	ReasonRevoked  = "revoked"
	ReasonReplaced = "replaced"
	ReasonLogout   = "logout"
	// ReasonSuperseded ends a session whose browser redeemed another
	// invitation.
	ReasonSuperseded = "superseded"
)

var (
	ErrInvalidToken    = errors.New("access: invalid or expired invitation")
	ErrUnauthenticated = errors.New("access: unauthenticated")
	ErrNotFound        = errors.New("access: not found")
	ErrInvalidArgument = errors.New("access: invalid argument")
)

// Listener is told about revocations after they are committed, so live
// connections can lose access without waiting to re-validate.
type Listener interface {
	GrantsRevoked(terminalID string, grantIDs []string, reason string)
	SessionRevoked(sessionID int64, reason string)
}

// Config wires a Service. Zero durations and limits receive defaults.
type Config struct {
	Store    *store.Store
	Listener Listener
	Now      func() time.Time
	// SessionTTL bounds an access session; it never outlives its grant.
	SessionTTL time.Duration
	// DefaultGrantTTL applies when a request gives no TTL; MaxGrantTTL is
	// the longest TTL a request may ask for.
	DefaultGrantTTL time.Duration
	MaxGrantTTL     time.Duration
	// MaxRedemptions is both the default and the upper bound of a grant's
	// redemption limit, and so of the sessions it can create.
	MaxRedemptions int
}

// Service manages grants and access sessions.
type Service struct {
	config Config
	// beforeCommit is a test seam run last inside every mutating transaction.
	beforeCommit func(context.Context) error
}

// NewService validates config and returns a Service.
func NewService(config Config) (*Service, error) {
	if config.Store == nil {
		return nil, errors.New("access: store is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.SessionTTL == 0 {
		config.SessionTTL = 12 * time.Hour
	}
	if config.MaxGrantTTL == 0 {
		config.MaxGrantTTL = 7 * 24 * time.Hour
	}
	if config.DefaultGrantTTL == 0 {
		config.DefaultGrantTTL = min(24*time.Hour, config.MaxGrantTTL)
	}
	if config.MaxRedemptions == 0 {
		config.MaxRedemptions = 100
	}
	switch {
	case config.SessionTTL < 0 || config.MaxRedemptions < 0:
		return nil, errors.New("access: limits must not be negative")
	case config.MaxGrantTTL < MinGrantTTL:
		return nil, fmt.Errorf("access: maximum grant TTL must be at least %v", MinGrantTTL)
	case config.DefaultGrantTTL < MinGrantTTL || config.DefaultGrantTTL > config.MaxGrantTTL:
		return nil, errors.New("access: default grant TTL must be between the minimum and maximum grant TTL")
	}
	return &Service{config: config}, nil
}

// Now returns the service clock's current time.
func (s *Service) Now() time.Time { return s.config.Now() }

// GrantRequest describes a new grant. A zero TTL or MaxRedemptions uses the
// configured default; SingleUse limits the grant to one redemption.
type GrantRequest struct {
	TerminalID     string
	Role           store.AccessRole
	Label          string
	TTL            time.Duration
	SingleUse      bool
	MaxRedemptions int
	RemoteAddr     string
}

// Invitation is a newly created grant. Token is the raw invitation token and
// is available only here. Replaced lists editor grants the new grant revoked.
type Invitation struct {
	Grant    store.AccessGrant
	Token    string
	Replaced []store.AccessGrant
}

// Session is a live access session. Token is set only by Redeem.
type Session struct {
	Token     string
	ID        int64
	Grant     store.AccessGrant
	ExpiresAt time.Time
}

// CreateGrant issues a grant for a terminal. Creating an editor grant
// atomically revokes the terminal's previous editor grant and its sessions.
func (s *Service) CreateGrant(ctx context.Context, req GrantRequest) (Invitation, error) {
	grant, err := s.normalize(req)
	if err != nil {
		return Invitation{}, err
	}
	token, err := randomToken(tokenBytes)
	if err != nil {
		return Invitation{}, err
	}
	var replaced []store.AccessGrant
	err = s.config.Store.WithTx(ctx, func(tx *store.Tx) error {
		if grant.Role == store.AccessEditor {
			var err error
			if replaced, err = tx.RevokeActiveEditorGrants(ctx, grant.TerminalID, grant.CreatedAt); err != nil {
				return err
			}
		}
		if err := tx.CreateAccessGrant(ctx, grant, HashToken(token)); err != nil {
			return err
		}
		for _, old := range replaced {
			if err := s.audit(ctx, tx, "access.grant.replaced", req.RemoteAddr, map[string]string{
				"terminalId": grant.TerminalID, "grantId": old.PublicID, "replacedBy": grant.PublicID,
			}); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, tx, "access.grant.created", req.RemoteAddr, map[string]string{
			"terminalId": grant.TerminalID, "grantId": grant.PublicID, "role": string(grant.Role),
			"singleUse": strconv.FormatBool(grant.SingleUse), "maxRedemptions": strconv.Itoa(grant.MaxRedemptions),
			"expiresAt": grant.ExpiresAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		return s.precommit(ctx)
	})
	if errors.Is(err, store.ErrNotFound) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, err
	}
	if len(replaced) > 0 {
		ids := make([]string, len(replaced))
		for i, old := range replaced {
			ids[i] = old.PublicID
		}
		s.notifyGrants(grant.TerminalID, ids, ReasonReplaced)
	}
	return Invitation{Grant: grant, Token: token, Replaced: replaced}, nil
}

func (s *Service) normalize(req GrantRequest) (store.AccessGrant, error) {
	if req.Role != store.AccessEditor && req.Role != store.AccessViewer {
		return store.AccessGrant{}, fmt.Errorf("%w: role must be editor or viewer", ErrInvalidArgument)
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = s.config.DefaultGrantTTL
	}
	if ttl < MinGrantTTL || ttl > s.config.MaxGrantTTL {
		return store.AccessGrant{}, fmt.Errorf("%w: ttl must be between %v and %v", ErrInvalidArgument, MinGrantTTL, s.config.MaxGrantTTL)
	}
	uses := req.MaxRedemptions
	switch {
	case uses < 0 || uses > s.config.MaxRedemptions:
		return store.AccessGrant{}, fmt.Errorf("%w: maxRedemptions must be between 1 and %d", ErrInvalidArgument, s.config.MaxRedemptions)
	case req.SingleUse && uses > 1:
		return store.AccessGrant{}, fmt.Errorf("%w: a single-use grant has one redemption", ErrInvalidArgument)
	case req.SingleUse:
		uses = 1
	case uses == 0:
		uses = s.config.MaxRedemptions
	}
	if !validLabel(req.Label) {
		return store.AccessGrant{}, fmt.Errorf("%w: label must be at most %d bytes of printable text", ErrInvalidArgument, MaxLabelBytes)
	}
	id, err := randomToken(grantIDBytes)
	if err != nil {
		return store.AccessGrant{}, err
	}
	now := s.config.Now()
	return store.AccessGrant{
		PublicID: id, TerminalID: req.TerminalID, Role: req.Role, Label: req.Label, SingleUse: req.SingleUse,
		MaxRedemptions: uses, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(ttl),
	}, nil
}

func validLabel(label string) bool {
	if len(label) > MaxLabelBytes || !utf8.ValidString(label) {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Grants lists a terminal's grants in creation order.
func (s *Service) Grants(ctx context.Context, terminalID string) ([]store.AccessGrant, error) {
	return s.config.Store.AccessGrants(ctx, terminalID)
}

// RevokeGrant revokes one of the terminal's grants and all its sessions.
// Revoking an already revoked grant returns it unchanged.
func (s *Service) RevokeGrant(ctx context.Context, terminalID, grantID, remoteAddr string) (store.AccessGrant, error) {
	var grant store.AccessGrant
	var changed bool
	err := s.config.Store.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		if grant, changed, err = tx.RevokeAccessGrant(ctx, terminalID, grantID, s.config.Now()); err != nil || !changed {
			return err
		}
		if err := s.audit(ctx, tx, "access.grant.revoked", remoteAddr, map[string]string{
			"terminalId": terminalID, "grantId": grantID, "role": string(grant.Role),
		}); err != nil {
			return err
		}
		return s.precommit(ctx)
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.AccessGrant{}, ErrNotFound
	}
	if err != nil {
		return store.AccessGrant{}, err
	}
	if changed {
		s.notifyGrants(terminalID, []string{grantID}, ReasonRevoked)
	}
	return grant, nil
}

// Redeem exchanges an invitation token for a new access session. The
// invitation itself never becomes a session credential. A non-nil check runs
// inside the redemption's transaction; its error rolls the redemption back,
// leaving the invitation unused.
func (s *Service) Redeem(ctx context.Context, token, remoteAddr string, check func(store.AccessGrant) error) (Session, error) {
	return s.RedeemReplacing(ctx, token, "", remoteAddr, check)
}

// RedeemReplacing is Redeem that also revokes the live access session
// identified by previous, if any, in the same transaction.
func (s *Service) RedeemReplacing(ctx context.Context, token, previous, remoteAddr string, check func(store.AccessGrant) error) (Session, error) {
	if !wellFormed(token) {
		return Session{}, ErrInvalidToken
	}
	var superseded *store.AccessSession
	sessionToken, err := randomToken(tokenBytes)
	if err != nil {
		return Session{}, err
	}
	now := s.config.Now()
	session := Session{Token: sessionToken}
	err = s.config.Store.WithTx(ctx, func(tx *store.Tx) error {
		grant, err := tx.RedeemAccessGrant(ctx, HashToken(token), now)
		if err != nil {
			return err
		}
		expires := now.Add(s.config.SessionTTL)
		if grant.ExpiresAt.Before(expires) {
			expires = grant.ExpiresAt
		}
		session.Grant, session.ExpiresAt = grant, expires
		if session.ID, err = tx.CreateAccessSession(ctx, HashToken(sessionToken), grant.PublicID, now, expires); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, "access.grant.redeemed", remoteAddr, map[string]string{
			"terminalId": grant.TerminalID, "grantId": grant.PublicID, "role": string(grant.Role),
			"redemptionCount": strconv.Itoa(grant.RedemptionCount),
		}); err != nil {
			return err
		}
		if wellFormed(previous) {
			old, err := tx.RevokeAccessSession(ctx, HashToken(previous), now)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return err
			default:
				superseded = &old
				if err := s.audit(ctx, tx, "access.session.superseded", remoteAddr, map[string]string{
					"terminalId": old.Grant.TerminalID, "grantId": old.Grant.PublicID, "role": string(old.Grant.Role),
					"replacedBy": grant.PublicID,
				}); err != nil {
					return err
				}
			}
		}
		if check != nil {
			if err := check(grant); err != nil {
				return err
			}
		}
		return s.precommit(ctx)
	})
	if errors.Is(err, store.ErrNotFound) {
		return Session{}, ErrInvalidToken
	}
	if err != nil {
		return Session{}, err
	}
	if superseded != nil && s.config.Listener != nil {
		s.config.Listener.SessionRevoked(superseded.ID, ReasonSuperseded)
	}
	return session, nil
}

// Authenticate resolves an access-session token to a live session.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if !wellFormed(token) {
		return Session{}, ErrUnauthenticated
	}
	record, err := s.config.Store.AccessSession(ctx, HashToken(token), s.config.Now())
	if errors.Is(err, store.ErrNotFound) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	return Session{ID: record.ID, Grant: record.Grant, ExpiresAt: record.ExpiresAt}, nil
}

// Logout revokes the access session identified by token.
func (s *Service) Logout(ctx context.Context, token, remoteAddr string) error {
	if !wellFormed(token) {
		return ErrUnauthenticated
	}
	var record store.AccessSession
	err := s.config.Store.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		if record, err = tx.RevokeAccessSession(ctx, HashToken(token), s.config.Now()); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, "access.logout", remoteAddr, map[string]string{
			"terminalId": record.Grant.TerminalID, "grantId": record.Grant.PublicID, "role": string(record.Grant.Role),
		}); err != nil {
			return err
		}
		return s.precommit(ctx)
	})
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	if s.config.Listener != nil {
		s.config.Listener.SessionRevoked(record.ID, ReasonLogout)
	}
	return nil
}

// Audit records a collaboration audit event that changes no capability
// state. details must never contain capability tokens, their hashes, or
// terminal content.
func (s *Service) Audit(ctx context.Context, eventType, remoteAddr string, details map[string]string) error {
	return s.config.Store.AppendAuditEvent(ctx, s.event(eventType, remoteAddr, details))
}

// audit records the audit event of a state change inside its transaction,
// so neither can be committed without the other.
func (s *Service) audit(ctx context.Context, tx *store.Tx, eventType, remoteAddr string, details map[string]string) error {
	return tx.AppendAuditEvent(ctx, s.event(eventType, remoteAddr, details))
}

func (s *Service) event(eventType, remoteAddr string, details map[string]string) store.AuditEvent {
	return store.AuditEvent{OccurredAt: s.config.Now(), Type: eventType, RemoteAddr: remoteAddr, Details: details}
}

func (s *Service) precommit(ctx context.Context) error {
	if s.beforeCommit == nil {
		return nil
	}
	return s.beforeCommit(ctx)
}

func (s *Service) notifyGrants(terminalID string, grantIDs []string, reason string) {
	if s.config.Listener != nil {
		s.config.Listener.GrantsRevoked(terminalID, grantIDs, reason)
	}
}

// Status summarizes whether grant can still be redeemed at now.
func Status(grant store.AccessGrant, now time.Time) string {
	switch {
	case !grant.RevokedAt.IsZero() && grant.RevokeReason == store.RevokeReasonReplaced:
		return StatusReplaced
	case !grant.RevokedAt.IsZero():
		return StatusRevoked
	case !now.Before(grant.ExpiresAt):
		return StatusExpired
	case grant.RedemptionCount >= grant.MaxRedemptions:
		return StatusExhausted
	default:
		return StatusActive
	}
}

// HashToken is the SHA-256 digest under which tokens are stored.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// CSRFToken derives the CSRF token bound to an access-session token.
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

// wellFormed rejects anything that cannot be a token before it is hashed or
// looked up.
func wellFormed(token string) bool {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil
}

func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("access: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
