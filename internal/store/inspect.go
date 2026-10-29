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
	db, err := openFile(path, inspectMode(path))
	if err != nil {
		return Inspection{}, err
	}
	defer db.Close()
	return inspectDB(ctx, db)
}

// inspectMode picks how to open path for reading. A WAL-mode reader creates
// -wal and -shm files, owned by whoever runs doctor. With neither present no
// server has the database open and the main file holds every commit, so it
// is read as immutable instead.
func inspectMode(path string) string {
	if !exists(path+"-wal") && !exists(path+"-shm") {
		return "immutable"
	}
	return "ro"
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// openFile opens an existing database file without migrations or the
// server's journal settings. mode is "ro", "rw", or "immutable" (read-only,
// without locks or WAL files); none creates the database file.
func openFile(path, mode string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", fileDSN(path, mode))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func fileDSN(path, mode string) string {
	query := url.Values{}
	if mode == "immutable" {
		mode = "ro"
		query.Set("immutable", "1")
	}
	query.Set("mode", mode)
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + query.Encode()
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

// maxIntegrityProblems bounds how many problems an integrity failure lists.
const maxIntegrityProblems = 3

func checkIntegrity(ctx context.Context, db *sql.DB) error {
	problems, err := integrityProblems(ctx, db)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("database integrity check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

func integrityProblems(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, fmt.Errorf("database integrity check: %w", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("database integrity check: %w", err)
		}
		if line != "ok" && len(problems) < maxIntegrityProblems {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database integrity check: %w", err)
	}
	return problems, nil
}

// appliedSchema verifies schema_migrations against this build and returns
// the newest applied version and how many are applied.
func appliedSchema(ctx context.Context, db *sql.DB) (version, applied int, err error) {
	if err := requireMigrationsTable(ctx, db); err != nil {
		return 0, 0, err
	}
	migrations, err := loadMigrations()
	if err != nil {
		return 0, 0, err
	}
	if version, applied, err = readApplied(ctx, db, migrations); err != nil {
		return 0, 0, err
	}
	if version != applied {
		return 0, 0, fmt.Errorf("database migrations are not contiguous: %d applied, newest is %d", applied, version)
	}
	return version, applied, nil
}

func readApplied(ctx context.Context, db *sql.DB, migrations []migration) (version, applied int, err error) {
	query, err := appliedQuery(ctx, db)
	if err != nil {
		return 0, 0, err
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	return checkAppliedRows(rows, migrations)
}

func requireMigrationsTable(ctx context.Context, db *sql.DB) error {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&present); err != nil {
		return fmt.Errorf("read database: %w", err)
	}
	if present == 0 {
		return errors.New("not a webpty database: it has no schema_migrations table")
	}
	return nil
}

// appliedQuery lists applied versions with their checksums; a database
// that predates checksums yields empty ones.
func appliedQuery(ctx context.Context, db *sql.DB) (string, error) {
	var hasChecksum int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('schema_migrations') WHERE name = 'checksum'`).Scan(&hasChecksum); err != nil {
		return "", fmt.Errorf("read schema_migrations: %w", err)
	}
	if hasChecksum > 0 {
		return `SELECT version, checksum FROM schema_migrations ORDER BY version`, nil
	}
	return `SELECT version, '' FROM schema_migrations ORDER BY version`, nil
}

// checkAppliedRows checks each applied row against this build's migrations
// and returns the newest version and how many rows there are.
func checkAppliedRows(rows *sql.Rows, migrations []migration) (version, applied int, err error) {
	known := migrationsByVersion(migrations)
	for rows.Next() {
		var v int
		var checksum string
		if err := rows.Scan(&v, &checksum); err != nil {
			return 0, 0, fmt.Errorf("read schema_migrations: %w", err)
		}
		if err := checkAppliedVersion(known, v, checksum); err != nil {
			return 0, 0, err
		}
		applied++
		version = max(version, v)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	return version, applied, nil
}

// checkAppliedVersion is the read-only counterpart of checkRecorded: it
// tells a database from a newer webpty apart from a corrupt one.
func checkAppliedVersion(known map[int]migration, v int, checksum string) error {
	m, ok := known[v]
	switch {
	case !ok && v > len(known):
		return fmt.Errorf("database has migration version %d, but this build supports up to %d: %w", v, len(known), ErrNewerSchema)
	case !ok:
		return fmt.Errorf("database has unknown migration version %d", v)
	case checksum != "" && checksum != m.checksum:
		return fmt.Errorf("migration %s changed after it was applied", m.name)
	}
	return nil
}
