package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

var (
	// ErrNewerSchema reports a database written by a newer webpty.
	ErrNewerSchema = errors.New("database schema is newer than this webpty supports")
	// ErrDatabaseInUse reports that another process holds the database lock.
	ErrDatabaseInUse = errors.New("database is in use by a running webpty; stop it first")
)

// LatestSchemaVersion is the newest migration version this build applies.
func LatestSchemaVersion() int {
	n, err := EmbeddedMigrations()
	if err != nil {
		panic(err)
	}
	return n
}

// EmbeddedMigrations checks that this build's migrations are named, ordered,
// and contiguous, and returns how many there are.
func EmbeddedMigrations() (int, error) {
	migrations, err := loadMigrations()
	return len(migrations), err
}

// Inspection describes a database file's schema without changing it.
type Inspection struct {
	Exists bool
	// SchemaVersion is the newest applied migration; 0 for an empty file.
	SchemaVersion int
	// Pending is how many migrations the server would apply on start.
	Pending int
}

// Inspect checks that the database at path is intact and that every
// applied migration is one this build knows, unchanged. It opens the file
// read-only and never creates or migrates it.
func Inspect(ctx context.Context, path string) (Inspection, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Inspection{}, nil
	}
	if err != nil {
		return Inspection{}, fmt.Errorf("inspect database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Inspection{}, fmt.Errorf("inspect database: %s is not a regular file", path)
	}
	// A WAL-mode reader creates -wal and -shm files, owned by whoever runs
	// doctor. With neither present no server has the database open and the
	// main file holds every commit, so it is read as immutable instead.
	mode := "ro"
	if !exists(path+"-wal") && !exists(path+"-shm") {
		mode = "immutable"
	}
	db, err := openFile(path, mode)
	if err != nil {
		return Inspection{}, err
	}
	defer db.Close()
	return inspectDB(ctx, db)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// openFile opens an existing database file without migrations or the
// server's journal settings. mode is "ro", "rw", or "immutable" (read-only,
// without locks or WAL files); none creates the database file.
func openFile(path, mode string) (*sql.DB, error) {
	query := url.Values{}
	if mode == "immutable" {
		mode = "ro"
		query.Set("immutable", "1")
	}
	query.Set("mode", mode)
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+escaped+"?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func inspectDB(ctx context.Context, db *sql.DB) (Inspection, error) {
	if err := checkIntegrity(ctx, db); err != nil {
		return Inspection{}, err
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master`).Scan(&tables); err != nil {
		return Inspection{}, fmt.Errorf("read database: %w", err)
	}
	latest := LatestSchemaVersion()
	if tables == 0 {
		return Inspection{Exists: true, Pending: latest}, nil
	}
	version, applied, err := appliedSchema(ctx, db)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{Exists: true, SchemaVersion: version, Pending: latest - applied}, nil
}

func checkIntegrity(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("database integrity check: %w", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("database integrity check: %w", err)
		}
		if line != "ok" && len(problems) < 3 {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("database integrity check: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("database integrity check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// appliedSchema verifies schema_migrations against this build and returns
// the newest applied version and how many are applied.
func appliedSchema(ctx context.Context, db *sql.DB) (version, applied int, err error) {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&present); err != nil {
		return 0, 0, fmt.Errorf("read database: %w", err)
	}
	if present == 0 {
		return 0, 0, errors.New("not a webpty database: it has no schema_migrations table")
	}
	migrations, err := loadMigrations()
	if err != nil {
		return 0, 0, err
	}
	known := make(map[int]migration, len(migrations))
	for _, m := range migrations {
		known[m.version] = m
	}
	var hasChecksum int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('schema_migrations') WHERE name = 'checksum'`).Scan(&hasChecksum); err != nil {
		return 0, 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	query := `SELECT version, '' FROM schema_migrations ORDER BY version`
	if hasChecksum > 0 {
		query = `SELECT version, checksum FROM schema_migrations ORDER BY version`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var checksum string
		if err := rows.Scan(&v, &checksum); err != nil {
			return 0, 0, fmt.Errorf("read schema_migrations: %w", err)
		}
		m, ok := known[v]
		switch {
		case !ok && v > len(migrations):
			return 0, 0, fmt.Errorf("database has migration version %d, but this build supports up to %d: %w", v, len(migrations), ErrNewerSchema)
		case !ok:
			return 0, 0, fmt.Errorf("database has unknown migration version %d", v)
		case checksum != "" && checksum != m.checksum:
			return 0, 0, fmt.Errorf("migration %s changed after it was applied", m.name)
		}
		applied++
		version = max(version, v)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	if version != applied {
		return 0, 0, fmt.Errorf("database migrations are not contiguous: %d applied, newest is %d", applied, version)
	}
	return version, applied, nil
}
