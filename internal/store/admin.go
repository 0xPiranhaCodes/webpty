package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AdminCredential is the single administrator's encoded password hash.
// Version increases on every write so callers can detect concurrent rotation.
type AdminCredential struct {
	PasswordHash string
	Version      int64
	UpdatedAt    time.Time
}

// SessionKind distinguishes bootstrap sessions from full admin sessions.
type SessionKind string

const (
	SessionKindBootstrap SessionKind = "bootstrap"
	SessionKindAdmin     SessionKind = "admin"
)

// AdminSession is a persisted session. TokenHash is the SHA-256 of the bearer token.
type AdminSession struct {
	TokenHash []byte
	Kind      SessionKind
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AdminCredential returns the stored credential, or ErrNotFound in bootstrap state.
func (s *Store) AdminCredential(ctx context.Context) (AdminCredential, error) {
	return adminCredential(ctx, s.db)
}

// AdminCredential returns the stored credential within the transaction.
func (t *Tx) AdminCredential(ctx context.Context) (AdminCredential, error) {
	return adminCredential(ctx, t.tx)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func adminCredential(ctx context.Context, q queryer) (AdminCredential, error) {
	var credential AdminCredential
	var updatedAt int64
	err := q.QueryRowContext(ctx, `SELECT password_hash, version, updated_at FROM admin_credentials WHERE id = 1`).
		Scan(&credential.PasswordHash, &credential.Version, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminCredential{}, ErrNotFound
	}
	if err != nil {
		return AdminCredential{}, fmt.Errorf("load admin credential: %w", err)
	}
	credential.UpdatedAt = fromDB(updatedAt)
	return credential, nil
}

// SetAdminCredential creates or replaces the administrator password hash.
func (t *Tx) SetAdminCredential(ctx context.Context, passwordHash string, updatedAt time.Time) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO admin_credentials (id, password_hash, updated_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET password_hash = excluded.password_hash, updated_at = excluded.updated_at,
			version = admin_credentials.version + 1`,
		passwordHash, toDB(updatedAt))
	if err != nil {
		return fmt.Errorf("store admin credential: %w", err)
	}
	return nil
}

// CreateAdminSession persists a session.
func (t *Tx) CreateAdminSession(ctx context.Context, session AdminSession) error {
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO admin_sessions (token_hash, kind, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		session.TokenHash, string(session.Kind), toDB(session.CreatedAt), toDB(session.ExpiresAt))
	if err != nil {
		return fmt.Errorf("create admin session: %w", err)
	}
	return nil
}

// DeleteAllAdminSessions revokes every bootstrap and admin session.
func (t *Tx) DeleteAllAdminSessions(ctx context.Context) error {
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM admin_sessions`); err != nil {
		return fmt.Errorf("delete admin sessions: %w", err)
	}
	return nil
}

// AdminSession returns the session whose token hash matches and which has not
// expired at now. Expired or unknown sessions return ErrNotFound.
func (s *Store) AdminSession(ctx context.Context, tokenHash []byte, now time.Time) (AdminSession, error) {
	return adminSession(ctx, s.db, tokenHash, now)
}

// AdminSession returns the live session within the transaction.
func (t *Tx) AdminSession(ctx context.Context, tokenHash []byte, now time.Time) (AdminSession, error) {
	return adminSession(ctx, t.tx, tokenHash, now)
}

func adminSession(ctx context.Context, q queryer, tokenHash []byte, now time.Time) (AdminSession, error) {
	session := AdminSession{TokenHash: tokenHash}
	var kind string
	var createdAt, expiresAt int64
	err := q.QueryRowContext(ctx,
		`SELECT kind, created_at, expires_at FROM admin_sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, toDB(now)).Scan(&kind, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminSession{}, ErrNotFound
	}
	if err != nil {
		return AdminSession{}, fmt.Errorf("load admin session: %w", err)
	}
	session.Kind = SessionKind(kind)
	session.CreatedAt = fromDB(createdAt)
	session.ExpiresAt = fromDB(expiresAt)
	return session, nil
}

// DeleteAdminSession revokes one session.
func (s *Store) DeleteAdminSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete admin session: %w", err)
	}
	return nil
}

// DeleteExpiredAdminSessions removes sessions that expired at or before now.
func (s *Store) DeleteExpiredAdminSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at <= ?`, toDB(now)); err != nil {
		return fmt.Errorf("delete expired admin sessions: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes admin, bootstrap, and access sessions that
// expired at or before now, and returns how many it removed. Expired
// sessions can never authenticate again, so removing them changes nothing
// but the table size.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	var removed int64
	err := s.WithTx(ctx, func(tx *Tx) error {
		for _, query := range []string{
			`DELETE FROM admin_sessions WHERE expires_at <= ?`,
			`DELETE FROM access_sessions WHERE expires_at <= ?`,
		} {
			result, err := tx.tx.ExecContext(ctx, query, toDB(now))
			if err != nil {
				return fmt.Errorf("delete expired sessions: %w", err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("delete expired sessions: %w", err)
			}
			removed += n
		}
		return nil
	})
	return removed, err
}
