// Package store owns the SQLite system of record.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound reports that a requested record does not exist or is no longer valid.
var ErrNotFound = errors.New("store: not found")

const busyTimeout = 5 * time.Second

// Store is a migrated SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, applies pragmas, and runs pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func dsn(path string) string {
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	query.Set("_txlock", "immediate")
	return path + "?" + query.Encode()
}

// DB exposes the underlying handle for diagnostics and tests.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Tx is a repository bound to a single transaction.
type Tx struct {
	tx *sql.Tx
}

// WithTx runs fn in a transaction, committing only when fn returns nil.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(&Tx{tx: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	// Once ctx is done database/sql rolls the transaction back on its own,
	// so the cancellation is reported instead of a bare ErrTxDone.
	if err := ctx.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func toDB(t time.Time) int64 { return t.UnixNano() }

func fromDB(v int64) time.Time { return time.Unix(0, v) }
