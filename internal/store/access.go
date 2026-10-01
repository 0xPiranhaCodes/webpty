package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AccessRole is the capability an access grant confers on a terminal.
type AccessRole string

const (
	AccessEditor AccessRole = "editor"
	AccessViewer AccessRole = "viewer"
)

// Grant revocation reasons.
const (
	RevokeReasonRevoked  = "revoked"
	RevokeReasonReplaced = "replaced"
)

// AccessGrant is a capability invitation for one terminal. Its token hash
// is write-only: it is never read back out of the store.
type AccessGrant struct {
	PublicID        string
	TerminalID      string
	Role            AccessRole
	Label           string
	SingleUse       bool
	MaxRedemptions  int
	RedemptionCount int
	RedeemedAt      time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExpiresAt       time.Time
	RevokedAt       time.Time
	RevokeReason    string
}

// AccessSession is a live bearer session created by redeeming a grant. ID is
// an internal row identifier; the bearer token is stored only as a hash.
type AccessSession struct {
	ID        int64
	Grant     AccessGrant
	CreatedAt time.Time
	ExpiresAt time.Time
}

const grantColumns = `g.public_id, t.public_id, g.role, g.label, g.single_use, g.max_redemptions, g.redemption_count,
	g.redeemed_at, g.created_at, g.updated_at, g.expires_at, g.revoked_at, g.revoke_reason`

const grantFrom = ` FROM access_grants g JOIN terminal_sessions t ON t.id = g.terminal_session_id`

// CreateAccessGrant inserts grant for the terminal named by grant.TerminalID,
// or returns ErrNotFound if that terminal does not exist.
func (t *Tx) CreateAccessGrant(ctx context.Context, grant AccessGrant, tokenHash []byte) error {
	result, err := t.tx.ExecContext(ctx, `INSERT INTO access_grants (public_id, terminal_session_id, token_hash, role,
			label, single_use, max_redemptions, created_at, updated_at, expires_at)
		SELECT ?, id, ?, ?, ?, ?, ?, ?, ?, ? FROM terminal_sessions WHERE public_id = ?`,
		grant.PublicID, tokenHash, string(grant.Role), grant.Label, grant.SingleUse, grant.MaxRedemptions,
		toDB(grant.CreatedAt), toDB(grant.UpdatedAt), toDB(grant.ExpiresAt), grant.TerminalID)
	if err != nil {
		return fmt.Errorf("create access grant: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("create access grant: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AccessGrants lists a terminal's grants in creation order.
func (s *Store) AccessGrants(ctx context.Context, terminalID string) ([]AccessGrant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+grantColumns+grantFrom+` WHERE t.public_id = ? ORDER BY g.id`, terminalID)
	if err != nil {
		return nil, fmt.Errorf("list access grants: %w", err)
	}
	defer rows.Close()
	var grants []AccessGrant
	for rows.Next() {
		grant, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// RedeemAccessGrant counts one redemption of the grant whose token hash
// matches, provided it is unrevoked, unexpired at now, and below its
// redemption limit. Otherwise it returns ErrNotFound.
func (t *Tx) RedeemAccessGrant(ctx context.Context, tokenHash []byte, now time.Time) (AccessGrant, error) {
	var publicID string
	err := t.tx.QueryRowContext(ctx, `UPDATE access_grants
		SET redemption_count = redemption_count + 1, redeemed_at = ?1, updated_at = ?1
		WHERE token_hash = ?2 AND revoked_at IS NULL AND expires_at > ?1 AND redemption_count < max_redemptions
		RETURNING public_id`, toDB(now), tokenHash).Scan(&publicID)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessGrant{}, ErrNotFound
	}
	if err != nil {
		return AccessGrant{}, fmt.Errorf("redeem access grant: %w", err)
	}
	return t.grant(ctx, `g.public_id = ?`, publicID)
}

// RevokeAccessGrant revokes the terminal's grant and all of its sessions.
// changed is false if the grant was already revoked. A grant of another
// terminal returns ErrNotFound.
func (t *Tx) RevokeAccessGrant(ctx context.Context, terminalID, grantID string, now time.Time) (grant AccessGrant, changed bool, err error) {
	grant, err = t.grant(ctx, `g.public_id = ? AND t.public_id = ?`, grantID, terminalID)
	if err != nil {
		return AccessGrant{}, false, err
	}
	if !grant.RevokedAt.IsZero() {
		return grant, false, nil
	}
	if err := t.revokeGrants(ctx, `public_id = ?`, []any{grantID}, RevokeReasonRevoked, now); err != nil {
		return AccessGrant{}, false, err
	}
	grant, err = t.grant(ctx, `g.public_id = ?`, grantID)
	return grant, true, err
}

// RevokeActiveEditorGrants revokes every unrevoked editor grant of the
// terminal, and their sessions, as replaced. It returns the revoked grants.
func (t *Tx) RevokeActiveEditorGrants(ctx context.Context, terminalID string, now time.Time) ([]AccessGrant, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT g.public_id`+grantFrom+`
		WHERE t.public_id = ? AND g.role = 'editor' AND g.revoked_at IS NULL`, terminalID)
	if err != nil {
		return nil, fmt.Errorf("find editor grants: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("find editor grants: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find editor grants: %w", err)
	}
	revoked := make([]AccessGrant, 0, len(ids))
	for _, id := range ids {
		if err := t.revokeGrants(ctx, `public_id = ?`, []any{id}, RevokeReasonReplaced, now); err != nil {
			return nil, err
		}
		grant, err := t.grant(ctx, `g.public_id = ?`, id)
		if err != nil {
			return nil, err
		}
		revoked = append(revoked, grant)
	}
	return revoked, nil
}

func (t *Tx) revokeGrants(ctx context.Context, where string, args []any, reason string, now time.Time) error {
	at := toDB(now)
	if _, err := t.tx.ExecContext(ctx, `UPDATE access_sessions SET revoked_at = ? WHERE revoked_at IS NULL
		AND grant_id IN (SELECT id FROM access_grants WHERE `+where+`)`, append([]any{at}, args...)...); err != nil {
		return fmt.Errorf("revoke access sessions: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoke_reason = ?, updated_at = ?
		WHERE revoked_at IS NULL AND `+where, append([]any{at, reason, at}, args...)...); err != nil {
		return fmt.Errorf("revoke access grant: %w", err)
	}
	return nil
}

func (t *Tx) grant(ctx context.Context, where string, args ...any) (AccessGrant, error) {
	grant, err := scanGrant(t.tx.QueryRowContext(ctx, `SELECT `+grantColumns+grantFrom+` WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessGrant{}, ErrNotFound
	}
	return grant, err
}

// CreateAccessSession persists a session for the grant. Storage rejects
// sessions for revoked or expired grants and sessions outliving the grant.
func (t *Tx) CreateAccessSession(ctx context.Context, tokenHash []byte, grantID string, createdAt, expiresAt time.Time) (int64, error) {
	result, err := t.tx.ExecContext(ctx, `INSERT INTO access_sessions (token_hash, grant_id, created_at, expires_at)
		SELECT ?, id, ?, ? FROM access_grants WHERE public_id = ?`,
		tokenHash, toDB(createdAt), toDB(expiresAt), grantID)
	if err != nil {
		return 0, fmt.Errorf("create access session: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return 0, fmt.Errorf("create access session: %w", ErrNotFound)
	}
	return result.LastInsertId()
}

const sessionQuery = `SELECT s.id, s.created_at, s.expires_at, ` + grantColumns + `
	FROM access_sessions s JOIN access_grants g ON g.id = s.grant_id JOIN terminal_sessions t ON t.id = g.terminal_session_id
	WHERE s.token_hash = ?1 AND s.revoked_at IS NULL AND s.expires_at > ?2 AND g.revoked_at IS NULL AND g.expires_at > ?2`

// AccessSession returns the session whose token hash matches, provided it
// and its grant are unrevoked and unexpired at now. Otherwise it returns
// ErrNotFound.
func (s *Store) AccessSession(ctx context.Context, tokenHash []byte, now time.Time) (AccessSession, error) {
	return scanAccessSession(s.db.QueryRowContext(ctx, sessionQuery, tokenHash, toDB(now)))
}

// RevokeAccessSession revokes the live session whose token hash matches and
// returns it, or returns ErrNotFound.
func (s *Store) RevokeAccessSession(ctx context.Context, tokenHash []byte, now time.Time) (AccessSession, error) {
	var session AccessSession
	err := s.WithTx(ctx, func(tx *Tx) error {
		var err error
		session, err = tx.RevokeAccessSession(ctx, tokenHash, now)
		return err
	})
	return session, err
}

// RevokeAccessSession revokes the live session whose token hash matches and
// returns it, or returns ErrNotFound.
func (t *Tx) RevokeAccessSession(ctx context.Context, tokenHash []byte, now time.Time) (AccessSession, error) {
	session, err := scanAccessSession(t.tx.QueryRowContext(ctx, sessionQuery, tokenHash, toDB(now)))
	if err != nil {
		return AccessSession{}, err
	}
	if _, err := t.tx.ExecContext(ctx, `UPDATE access_sessions SET revoked_at = ? WHERE id = ?`, toDB(now), session.ID); err != nil {
		return AccessSession{}, fmt.Errorf("revoke access session: %w", err)
	}
	return session, nil
}

func scanAccessSession(row rowScanner) (AccessSession, error) {
	var session AccessSession
	var createdAt, expiresAt int64
	grant, err := scanGrantWith(row, &session.ID, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessSession{}, ErrNotFound
	}
	if err != nil {
		return AccessSession{}, err
	}
	session.Grant = grant
	session.CreatedAt = fromDB(createdAt)
	session.ExpiresAt = fromDB(expiresAt)
	return session, nil
}

func scanGrant(row rowScanner) (AccessGrant, error) { return scanGrantWith(row) }

func scanGrantWith(row rowScanner, leading ...any) (AccessGrant, error) {
	var grant AccessGrant
	var role string
	var createdAt, updatedAt, expiresAt int64
	var redeemedAt, revokedAt sql.NullInt64
	dest := append(leading, &grant.PublicID, &grant.TerminalID, &role, &grant.Label, &grant.SingleUse,
		&grant.MaxRedemptions, &grant.RedemptionCount, &redeemedAt, &createdAt, &updatedAt, &expiresAt,
		&revokedAt, &grant.RevokeReason)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AccessGrant{}, err
		}
		return AccessGrant{}, fmt.Errorf("scan access grant: %w", err)
	}
	grant.Role = AccessRole(role)
	grant.CreatedAt = fromDB(createdAt)
	grant.UpdatedAt = fromDB(updatedAt)
	grant.ExpiresAt = fromDB(expiresAt)
	if redeemedAt.Valid {
		grant.RedeemedAt = fromDB(redeemedAt.Int64)
	}
	if revokedAt.Valid {
		grant.RevokedAt = fromDB(revokedAt.Int64)
	}
	return grant, nil
}
