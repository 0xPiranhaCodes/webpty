package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func migrationFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys["migrations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "m.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func runMigrations(t *testing.T, db *sql.DB, files map[string]string) error {
	t.Helper()
	migrations, err := loadMigrationsFrom(migrationFS(files))
	if err != nil {
		return err
	}
	return applyMigrations(context.Background(), db, migrations, 1<<30)
}

func wantError(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("err = %v, want one mentioning %q", err, fragment)
	}
}

func TestMigrationVersionsMustBeContiguous(t *testing.T) {
	_, err := loadMigrationsFrom(migrationFS(map[string]string{
		"0001_a.sql": "CREATE TABLE a (x INTEGER);", "0003_c.sql": "CREATE TABLE c (x INTEGER);",
	}))
	wantError(t, err, "missing migration version 2")
}

func TestModifiedAppliedMigrationIsRejected(t *testing.T) {
	db := rawDB(t)
	if err := runMigrations(t, db, map[string]string{"0001_a.sql": "CREATE TABLE a (x INTEGER);"}); err != nil {
		t.Fatal(err)
	}
	err := runMigrations(t, db, map[string]string{"0001_a.sql": "CREATE TABLE a (x INTEGER, y INTEGER);"})
	wantError(t, err, "changed after it was applied")
}

func TestUnknownAppliedMigrationIsRejected(t *testing.T) {
	db := rawDB(t)
	if err := runMigrations(t, db, map[string]string{
		"0001_a.sql": "CREATE TABLE a (x INTEGER);", "0002_b.sql": "CREATE TABLE b (x INTEGER);",
	}); err != nil {
		t.Fatal(err)
	}
	err := runMigrations(t, db, map[string]string{"0001_a.sql": "CREATE TABLE a (x INTEGER);"})
	wantError(t, err, "newer webpty")
}

func TestLegacyMigrationRowsAreBackfilledWithChecksums(t *testing.T) {
	db := rawDB(t)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL) STRICT;
		CREATE TABLE a (x INTEGER);
		INSERT INTO schema_migrations VALUES (1, '0001_a.sql', 1)`); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"0001_a.sql": "CREATE TABLE a (x INTEGER);", "0002_b.sql": "CREATE TABLE b (x INTEGER);"}
	if err := runMigrations(t, db, files); err != nil {
		t.Fatal(err)
	}
	var blank int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE checksum = ''`).Scan(&blank); err != nil || blank != 0 {
		t.Fatalf("rows without checksum = %d, %v", blank, err)
	}
	files["0001_a.sql"] = "CREATE TABLE a (x TEXT);"
	wantError(t, runMigrations(t, db, files), "changed after it was applied")
}
