package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TerminalState is the lifecycle state of a terminal session.
type TerminalState string

const (
	TerminalStarting   TerminalState = "starting"
	TerminalRunning    TerminalState = "running"
	TerminalExited     TerminalState = "exited"
	TerminalFailed     TerminalState = "failed"
	TerminalTerminated TerminalState = "terminated"
)

// TerminalSession is the persisted metadata of one PTY session. Zero times
// are stored as NULL. Terminal input and output are never persisted here.
type TerminalSession struct {
	PublicID       string
	Title          string
	State          TerminalState
	Command        string
	Args           []string
	Rows, Cols     int
	CreatedAt      time.Time
	StartedAt      time.Time
	EndedAt        time.Time
	LastActivityAt time.Time
	ExitCode       *int
	ExitSignal     string
	Failure        string
}

const terminalColumns = `public_id, title, state, command, args, rows, cols, created_at, started_at, ended_at,
	last_activity_at, exit_code, exit_signal, failure`

// CreateTerminalSession inserts a new session.
func (s *Store) CreateTerminalSession(ctx context.Context, session TerminalSession) error {
	args, err := encodeArgs(session.Args)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO terminal_sessions (`+terminalColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.PublicID, session.Title, string(session.State), session.Command, args, session.Rows, session.Cols,
		toDB(session.CreatedAt), nullTime(session.StartedAt), nullTime(session.EndedAt),
		toDB(session.LastActivityAt), nullInt(session.ExitCode), session.ExitSignal, session.Failure)
	if err != nil {
		return fmt.Errorf("create terminal session: %w", err)
	}
	return nil
}

// UpdateTerminalSession replaces the mutable fields of the session with the
// same PublicID, or returns ErrNotFound.
func (s *Store) UpdateTerminalSession(ctx context.Context, session TerminalSession) error {
	result, err := s.db.ExecContext(ctx, `UPDATE terminal_sessions SET state = ?, rows = ?, cols = ?, started_at = ?,
		ended_at = ?, last_activity_at = ?, exit_code = ?, exit_signal = ?, failure = ? WHERE public_id = ?`,
		string(session.State), session.Rows, session.Cols, nullTime(session.StartedAt), nullTime(session.EndedAt),
		toDB(session.LastActivityAt), nullInt(session.ExitCode), session.ExitSignal, session.Failure, session.PublicID)
	if err != nil {
		return fmt.Errorf("update terminal session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update terminal session: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// TerminalSession returns the session with publicID, or ErrNotFound.
func (s *Store) TerminalSession(ctx context.Context, publicID string) (TerminalSession, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+terminalColumns+` FROM terminal_sessions WHERE public_id = ?`, publicID)
	session, err := scanTerminalSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TerminalSession{}, ErrNotFound
	}
	return session, err
}

// TerminalSessions lists sessions in creation order.
func (s *Store) TerminalSessions(ctx context.Context) ([]TerminalSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+terminalColumns+` FROM terminal_sessions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list terminal sessions: %w", err)
	}
	defer rows.Close()
	var sessions []TerminalSession
	for rows.Next() {
		session, err := scanTerminalSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// FailInterruptedTerminalSessions marks sessions left starting or running by
// a previous process as failed. Their processes cannot be reattached.
func (s *Store) FailInterruptedTerminalSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE terminal_sessions SET state = 'failed', ended_at = ?,
		failure = 'server restarted' WHERE state IN ('starting', 'running')`, toDB(now))
	if err != nil {
		return 0, fmt.Errorf("fail interrupted terminal sessions: %w", err)
	}
	return result.RowsAffected()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTerminalSession(row rowScanner) (TerminalSession, error) {
	var session TerminalSession
	var state, args string
	var createdAt, lastActivityAt int64
	var startedAt, endedAt, exitCode sql.NullInt64
	err := row.Scan(&session.PublicID, &session.Title, &state, &session.Command, &args, &session.Rows, &session.Cols,
		&createdAt, &startedAt, &endedAt, &lastActivityAt, &exitCode, &session.ExitSignal, &session.Failure)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TerminalSession{}, err
		}
		return TerminalSession{}, fmt.Errorf("scan terminal session: %w", err)
	}
	if err := json.Unmarshal([]byte(args), &session.Args); err != nil {
		return TerminalSession{}, fmt.Errorf("decode terminal args: %w", err)
	}
	session.State = TerminalState(state)
	session.CreatedAt = fromDB(createdAt)
	session.LastActivityAt = fromDB(lastActivityAt)
	if startedAt.Valid {
		session.StartedAt = fromDB(startedAt.Int64)
	}
	if endedAt.Valid {
		session.EndedAt = fromDB(endedAt.Int64)
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		session.ExitCode = &code
	}
	return session, nil
}

func encodeArgs(args []string) (string, error) {
	if args == nil {
		args = []string{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode terminal args: %w", err)
	}
	return string(encoded), nil
}

func nullTime(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toDB(t), Valid: true}
}

func nullInt(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}
