package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

func loadMigrations() ([]migration, error) { return loadMigrationsFrom(migrationFiles) }

func loadMigrationsFrom(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: name must be NNNN_description.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q: invalid version", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, name, version)
		}
		seen[version] = name
		body, err := fs.ReadFile(fsys, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, migration{version: version, name: name, sql: string(body), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for i, m := range migrations {
		if m.version != i+1 {
			return nil, fmt.Errorf("missing migration version %d before %q", i+1, m.name)
		}
	}
	return migrations, nil
}

// foreignKeysOffDirective marks a migration that rebuilds a parent table.
// SQLite ignores PRAGMA foreign_keys inside a transaction, and dropping a
// parent table with enforcement on would cascade-delete its dependents, so
// such migrations run on a connection with enforcement disabled and must pass
// PRAGMA foreign_key_check before committing.
const foreignKeysOffDirective = "-- webpty:foreign-keys=off"

func migrate(ctx context.Context, db *sql.DB) error {
	return migrateTo(ctx, db, math.MaxInt)
}

func migrateTo(ctx context.Context, db *sql.DB, maxVersion int) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	return applyMigrations(ctx, db, migrations, maxVersion)
}

// applyMigrations applies migrations up to maxVersion after checking that
// every recorded migration is known and unchanged. Rows recorded before
// checksums existed are trusted once and given the current checksum.
func applyMigrations(ctx context.Context, db *sql.DB, migrations []migration, maxVersion int) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL,
		checksum TEXT NOT NULL DEFAULT ''
	) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if err := addChecksumColumn(ctx, db); err != nil {
		return err
	}
	if err := verifyApplied(ctx, db, migrations); err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version > maxVersion {
			break
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	defer conn.Close()

	disableForeignKeys := strings.HasPrefix(m.sql, foreignKeysOffDirective)
	if disableForeignKeys {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		defer func() {
			if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`); err != nil {
				// A pooled connection without enforcement must never be reused.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}()
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&applied); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if disableForeignKeys {
		if err := checkForeignKeys(ctx, tx); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at, checksum) VALUES (?, ?, ?, ?)`,
		m.version, m.name, toDB(time.Now()), m.checksum,
	); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	return nil
}

func addChecksumColumn(ctx context.Context, db *sql.DB) error {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('schema_migrations') WHERE name = 'checksum'`).Scan(&present); err != nil {
		return fmt.Errorf("inspect schema_migrations: %w", err)
	}
	if present > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN checksum TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add migration checksums: %w", err)
	}
	return nil
}

func verifyApplied(ctx context.Context, db *sql.DB, migrations []migration) error {
	known := make(map[int]migration, len(migrations))
	for _, m := range migrations {
		known[m.version] = m
	}
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	var legacy []migration
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		m, ok := known[version]
		switch {
		case !ok:
			rows.Close()
			return fmt.Errorf("database has migration version %d, which this build does not know; it was used by a newer webpty", version)
		case checksum == "":
			legacy = append(legacy, m)
		case checksum != m.checksum:
			rows.Close()
			return fmt.Errorf("migration %s changed after it was applied", m.name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for _, m := range legacy {
		if _, err := db.ExecContext(ctx, `UPDATE schema_migrations SET checksum = ? WHERE version = ? AND checksum = ''`,
			m.checksum, m.version); err != nil {
			return fmt.Errorf("record checksum for %s: %w", m.name, err)
		}
	}
	return nil
}

func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign key violations after migration")
	}
	return rows.Err()
}
